package controller

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/willuhmjs/heirloom/api/v1"
	"github.com/willuhmjs/heirloom/internal/jobcfg"
)

const (
	condReady   = "Ready"
	condHealthy = "Healthy"
)

func metaTime(t time.Time) *metav1.Time {
	mt := metav1.NewTime(t.Truncate(time.Second)) // API timestamps have second precision; keep status stable
	return &mt
}

// deriveStatus recomputes the run-derived parts of the policy status from observed Jobs and Pods.
// Fields with no supporting Job (history pruned) keep their previous value.
func deriveStatus(p *v1.BackupPolicy, jobs []batchv1.Job, pods []corev1.Pod, now time.Time) {
	st := &p.Status
	backups := finishedRuns(jobs, pods, jobcfg.ModeBackup)
	applySuccessfulBackup(st, backups) // must precede healthCondition: staleness reads status.lastSyncTime
	meta.SetStatusCondition(&st.Conditions, healthCondition(p, backups, now))
	applyManual(st, p.Spec.Trigger.Manual, p.Generation, jobs)

	if p.Spec.Verify == nil {
		st.LastVerify = nil
	} else if verifies := finishedRuns(jobs, pods, jobcfg.ModeVerify); len(verifies) > 0 {
		last := verifies[len(verifies)-1]
		st.LastVerify = &v1.VerifyStatus{
			Time:     *metaTime(last.at),
			OK:       last.ok,
			Snapshot: last.result.Snapshot,
			Message:  last.result.Message,
		}
	}
	st.NextSyncTime = nextSync(p, now)
}

// applySuccessfulBackup copies the newest successful run into the status.
func applySuccessfulBackup(st *v1.BackupPolicyStatus, backups []run) {
	for i := len(backups) - 1; i >= 0; i-- {
		if backups[i].ok {
			applySuccess(st, backups[i])
			return
		}
	}
}

// healthCondition is the single source of the Healthy condition: the outcome of the latest finished backup,
// overridden by Stale when a run is overdue. It is computed once so the condition never flips back and forth
// within one reconcile (which would change lastTransitionTime every time and re-trigger reconciles).
func healthCondition(p *v1.BackupPolicy, backups []run, now time.Time) metav1.Condition {
	cond := metav1.Condition{Type: condHealthy, ObservedGeneration: p.Generation}
	switch {
	case len(backups) == 0:
		if prev := meta.FindStatusCondition(p.Status.Conditions, condHealthy); prev != nil {
			cond = *prev.DeepCopy() // history was pruned: keep what we knew
		} else {
			cond.Status, cond.Reason, cond.Message = metav1.ConditionUnknown, "NoRuns", "no backup has finished yet"
		}
	case !backups[len(backups)-1].ok:
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "BackupFailed", backups[len(backups)-1].result.Message
	default:
		cond.Status, cond.Reason = metav1.ConditionTrue, "BackupSucceeded"
		// The runner's one-line summary (files, bytes, what restic added to the repository after dedup)
		// is the health message, so kubectl describe shows what the last backup actually cost.
		if msg := backups[len(backups)-1].result.Message; msg != "" {
			cond.Message = msg
		} else {
			cond.Message = "latest backup succeeded"
		}
	}
	if cond.Status != metav1.ConditionFalse {
		if at, ok := staleAt(p); ok && now.After(at) {
			cond.Status, cond.Reason = metav1.ConditionFalse, "Stale"
			if p.Status.LastSyncTime == nil {
				cond.Message = fmt.Sprintf("no backup has succeeded yet; the first was due by %s", at.UTC().Format(time.RFC3339))
			} else {
				cond.Message = fmt.Sprintf("no successful backup since %s; one was due by %s",
					p.Status.LastSyncTime.UTC().Format(time.RFC3339), at.UTC().Format(time.RFC3339))
			}
		}
	}
	return cond
}

const (
	minStaleGrace = 5 * time.Minute
	maxStaleGrace = 6 * time.Hour
)

// staleAt is the moment a policy becomes unhealthy if no backup succeeds: the first scheduled run after the
// last success, plus a grace period of a quarter of the schedule interval (clamped to 5m..6h). This catches
// failures that leave no finished Job, such as an unreachable NFS server holding the pod in ContainerCreating.
func staleAt(p *v1.BackupPolicy) (time.Time, bool) {
	if p.Spec.Suspend {
		return time.Time{}, false
	}
	sched, err := parseSchedule(p.Spec.Schedule, p.Spec.TimeZone)
	if err != nil {
		return time.Time{}, false
	}
	// Measured from the last success, or from creation for a policy that never succeeded: a backup that has
	// never worked is the one most worth hearing about.
	base := p.CreationTimestamp.Time
	if p.Status.LastSyncTime != nil {
		base = p.Status.LastSyncTime.Time
	}
	due := sched.Next(base)
	grace := min(max(sched.Next(due).Sub(due)/4, minStaleGrace), maxStaleGrace)
	return due.Add(grace), true
}

func applySuccess(st *v1.BackupPolicyStatus, r run) {
	res := r.result
	snapTime := res.Time
	if snapTime.IsZero() {
		snapTime = r.at
	}
	st.LastSnapshot = &v1.SnapshotInfo{Name: res.Snapshot, Time: *metaTime(snapTime), Bytes: res.Bytes, Files: res.Files}
	st.LastSyncTime = metaTime(r.at)
	st.LastSyncStartTime, st.LastSyncDuration = nil, nil
	if s := r.job.Status.StartTime; s != nil {
		st.LastSyncStartTime = metaTime(s.Time)
	}
	if res.Duration > 0 {
		st.LastSyncDuration = &metav1.Duration{Duration: (time.Duration(res.Duration * float64(time.Second))).Round(time.Millisecond)}
	}
	st.SnapshotsKept = int32(res.Kept)
}

// applyManual records the trigger value once its manual Job has succeeded.
func applyManual(st *v1.BackupPolicyStatus, value string, generation int64, jobs []batchv1.Job) {
	if value == "" {
		return
	}
	for i := range jobs {
		// Only the Job started for this value in this generation counts, not an old one that used the same value.
		if jobs[i].Annotations[annotationManual] != value || jobs[i].Annotations[annotationManualGen] != strconv.FormatInt(generation, 10) {
			continue
		}
		if _, ok := jobState(&jobs[i]); ok {
			st.LastManualSync = value
		}
	}
}

func nextSync(p *v1.BackupPolicy, now time.Time) *metav1.Time {
	if p.Spec.Suspend {
		return nil
	}
	sched, err := parseSchedule(p.Spec.Schedule, p.Spec.TimeZone)
	if err != nil {
		return nil
	}
	return metaTime(sched.Next(now).Add(jitterOffset(p, jobcfg.ModeBackup)))
}

func setReady(p *v1.BackupPolicy, ok bool, reason, message string) {
	status := metav1.ConditionTrue
	if !ok {
		status = metav1.ConditionFalse
	}
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: condReady, Status: status, Reason: reason, Message: message, ObservedGeneration: p.Generation,
	})
}

// maxPolicyNameLen keeps the longest Job name, "bs-<policy>-verify-<unix seconds>", within the 63 character limit.
const maxPolicyNameLen = 40

// validatePolicy checks the parts of a spec the CRD schema cannot express.
func validatePolicy(p *v1.BackupPolicy) error {
	if len(p.Name) > maxPolicyNameLen {
		return fmt.Errorf("policy name longer than %d characters", maxPolicyNameLen)
	}
	if err := validateSource(p.Spec.Source); err != nil {
		return err
	}
	if tz := p.Spec.TimeZone; tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			return fmt.Errorf("timeZone %q: %w", tz, err)
		}
	}
	if err := validateExpr("schedule", p.Spec.Schedule); err != nil {
		return err
	}
	if _, err := parseSchedule(p.Spec.Schedule, p.Spec.TimeZone); err != nil {
		return fmt.Errorf("schedule: %w", err)
	}
	if j := p.Spec.Jitter; j != "" {
		d, err := time.ParseDuration(j)
		if err != nil {
			return fmt.Errorf("jitter %q: %w", j, err)
		}
		if d < 0 || d > maxJitter {
			return fmt.Errorf("jitter %s: must be between 0 and %s", j, maxJitter)
		}
	}
	if v := p.Spec.Verify; v != nil {
		if err := validateExpr("verify.schedule", v.Schedule); err != nil {
			return err
		}
		if _, err := parseSchedule(v.Schedule, p.Spec.TimeZone); err != nil {
			return fmt.Errorf("verify.schedule: %w", err)
		}
		if pg := p.Spec.Source.Postgres; pg != nil && len(v.SQL) > 0 && v.Database == "" && len(pg.Databases) != 1 {
			return fmt.Errorf("verify.database is required when verify.sql is set and the policy does not list exactly one database")
		}
	}
	return nil
}

// validateExpr rejects TZ= / CRON_TZ= prefixes: the zone is spec.timeZone, one place for both schedules.
func validateExpr(field, expr string) error {
	trimmed := strings.TrimSpace(expr)
	if strings.HasPrefix(trimmed, "TZ=") || strings.HasPrefix(trimmed, "CRON_TZ=") {
		return fmt.Errorf("%s: set spec.timeZone instead of a TZ= prefix", field)
	}
	return nil
}
