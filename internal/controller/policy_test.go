package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/willuhmjs/backshots/api/v1alpha1"
	"github.com/willuhmjs/backshots/internal/jobcfg"
	"github.com/willuhmjs/backshots/internal/snapshot"
)

func policyDue(p *v1alpha1.BackupPolicy) *v1alpha1.BackupPolicy {
	p.CreationTimestamp = metav1.NewTime(testNow.Add(-2 * time.Hour)) // the 09:00 slot has passed
	return p
}

func jobsOf(t *testing.T, c client.Client, role jobcfg.Mode) []batchv1.Job {
	t.Helper()
	var list batchv1.JobList
	if err := c.List(context.Background(), &list, client.InNamespace("apps"), client.MatchingLabels{jobcfg.LabelRole: string(role)}); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

func onlyJob(t *testing.T, c client.Client, role jobcfg.Mode) *batchv1.Job {
	t.Helper()
	jobs := jobsOf(t, c, role)
	if len(jobs) != 1 {
		t.Fatalf("%s jobs = %d, want 1", role, len(jobs))
	}
	return &jobs[0]
}

func volume(pod corev1.PodSpec, name string) *corev1.Volume {
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == name {
			return &pod.Volumes[i]
		}
	}
	return nil
}

func mount(c corev1.Container, name string) *corev1.VolumeMount {
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == name {
			return &c.VolumeMounts[i]
		}
	}
	return nil
}

func TestScheduledBackupJob(t *testing.T) {
	c := newFake(t, testStore(), policyDue(filesPolicy()))
	reconcileKey(t, newPolicyReconciler(c), "apps", "app")
	cj := onlyJob(t, c, jobcfg.ModeBackup)
	if want := scheduledJobName("app", jobcfg.ModeBackup, time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)); cj.Name != want {
		t.Errorf("job name = %s, want %s", cj.Name, want)
	}
	js := cj.Spec
	if *js.BackoffLimit != 1 || *js.ActiveDeadlineSeconds != 3600 {
		t.Errorf("backoff/deadline = %d/%d", *js.BackoffLimit, *js.ActiveDeadlineSeconds)
	}
	if cj.Labels[jobcfg.LabelPolicy] != "app" || cj.Labels[jobcfg.LabelRole] != "backup" {
		t.Errorf("labels = %v", cj.Labels)
	}
	if ref := metav1.GetControllerOf(cj); ref == nil || ref.Name != "app" || ref.Kind != "BackupPolicy" {
		t.Errorf("owner = %v", ref)
	}
	pod := js.Template.Spec
	if pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %s", pod.RestartPolicy)
	}
	init := pod.InitContainers[0]
	if init.Name != "install" || init.Image != testImage || strings.Join(init.Command, " ") != "/backshots install /tools/backshots" {
		t.Errorf("init = %+v", init)
	}
	mover := pod.Containers[0]
	if mover.Name != "mover" || mover.Image != testImage || strings.Join(mover.Command, " ") != "/tools/backshots run" {
		t.Errorf("mover = %+v", mover)
	}
	sc := mover.SecurityContext
	if *sc.RunAsUser != 0 || *sc.AllowPrivilegeEscalation || sc.Capabilities == nil ||
		len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" || sc.SeccompProfile == nil {
		t.Errorf("securityContext = %+v", sc)
	}
	for _, c := range sc.Capabilities.Add {
		switch c {
		case "CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL", "SETUID", "SETGID":
		default:
			t.Errorf("unexpected capability %s", c)
		}
	}
	// The mover must only see its own policy directory, never the whole export.
	if got := mount(mover, "store").SubPath; got != "apps/app" {
		t.Errorf("store mount subPath = %q, want apps/app", got)
	}
	if mover.TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
		t.Errorf("termination policy = %s", mover.TerminationMessagePolicy)
	}
	nfs := volume(pod, "store").NFS
	if nfs.Server != "10.0.0.2" || nfs.Path != "/volume1/k8s" || nfs.ReadOnly || mount(mover, "store").ReadOnly {
		t.Errorf("backup store must be rw nfs: %+v", nfs)
	}
	if volume(pod, "tools").EmptyDir == nil || mount(mover, "tools").MountPath != "/tools" {
		t.Error("tools emptyDir missing")
	}
}

func TestRenderSourceTypes(t *testing.T) {
	tests := []struct {
		name         string
		policy       *v1alpha1.BackupPolicy
		image        string
		claim        string
		sourceRO     bool
		wantPassword bool
	}{
		{"files", filesPolicy(), testImage, "app-data", true, false},
		{"sqlite", sqlitePolicy(), testImage, "db", false, false},
		{"postgres", postgresPolicy(), "postgres:16", "", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newFake(t, testStore(), policyDue(tt.policy))
			reconcileKey(t, newPolicyReconciler(c), "apps", "app")
			pod := onlyJob(t, c, jobcfg.ModeBackup).Spec.Template.Spec
			mover := pod.Containers[0]
			if mover.Image != tt.image {
				t.Errorf("image = %s", mover.Image)
			}
			src := volume(pod, "source")
			if tt.claim == "" {
				if src != nil || mount(mover, "source") != nil {
					t.Error("postgres must not mount a source volume")
				}
			} else {
				pvc := src.PersistentVolumeClaim
				if pvc.ClaimName != tt.claim || pvc.ReadOnly != tt.sourceRO || mount(mover, "source").ReadOnly != tt.sourceRO || mount(mover, "source").MountPath != "/source" {
					t.Errorf("source volume = %+v", src)
				}
			}
			var pw *corev1.EnvVar
			for i := range mover.Env {
				if mover.Env[i].Name == "PGPASSWORD" {
					pw = &mover.Env[i]
				}
			}
			if (pw != nil) != tt.wantPassword {
				t.Fatalf("PGPASSWORD present = %v", pw != nil)
			}
			if pw != nil && (pw.ValueFrom.SecretKeyRef.Name != "pg" || pw.ValueFrom.SecretKeyRef.Key != "password") {
				t.Errorf("PGPASSWORD = %+v", pw)
			}
			if mover.Env[0].Name != "BS_CONFIG" || !strings.Contains(mover.Env[0].Value, `"mode":"backup"`) {
				t.Errorf("BS_CONFIG = %q", mover.Env[0].Value)
			}
		})
	}
}

func TestPodOptionsPropagate(t *testing.T) {
	p := filesPolicy()
	p.Spec.Pod = v1alpha1.PodOptions{
		NodeSelector:      map[string]string{"disk": "fast"},
		Tolerations:       []corev1.Toleration{{Key: "k", Operator: corev1.TolerationOpExists}},
		PriorityClassName: "low",
	}
	p.Spec.ActiveDeadlineSeconds = ptr.To[int64](120)
	c := newFake(t, testStore(), policyDue(p))
	reconcileKey(t, newPolicyReconciler(c), "apps", "app")
	js := onlyJob(t, c, jobcfg.ModeBackup).Spec
	pod := js.Template.Spec
	if pod.NodeSelector["disk"] != "fast" || len(pod.Tolerations) != 1 || pod.PriorityClassName != "low" || *js.ActiveDeadlineSeconds != 120 {
		t.Errorf("pod options not propagated: %+v", js)
	}
}

func TestRetentionResolutionOrder(t *testing.T) {
	policyRet := &v1alpha1.Retention{Daily: 5}
	storeRet := &v1alpha1.Retention{Weekly: 7}
	store := func(r *v1alpha1.Retention) *v1alpha1.BackupStore {
		s := testStore()
		s.Spec.Retention = r
		return s
	}
	if got := resolveRetention(policyRet, store(storeRet)); got != (snapshot.Retention{Daily: 5}) {
		t.Errorf("policy should win: %+v", got)
	}
	if got := resolveRetention(nil, store(storeRet)); got != (snapshot.Retention{Weekly: 7}) {
		t.Errorf("store should win: %+v", got)
	}
	if got := resolveRetention(nil, store(nil)); got != snapshot.Default {
		t.Errorf("default expected: %+v", got)
	}
}

func TestRetentionInConfig(t *testing.T) {
	s := testStore()
	s.Spec.Retention = &v1alpha1.Retention{Monthly: 3}
	c := newFake(t, s, policyDue(filesPolicy()))
	reconcileKey(t, newPolicyReconciler(c), "apps", "app")
	env := onlyJob(t, c, jobcfg.ModeBackup).Spec.Template.Spec.Containers[0].Env[0].Value
	if !strings.Contains(env, `"retention":{"Hourly":0,"Daily":0,"Weekly":0,"Monthly":3}`) {
		t.Errorf("config = %s", env)
	}
}

func TestVerifyJobScheduling(t *testing.T) {
	p := policyDue(postgresPolicy())
	p.Spec.Verify = &v1alpha1.Verify{Schedule: "30 * * * *", Database: "app"} // the 08:30 slot has passed
	p.Spec.Pod.ScratchStorageClassName = ptr.To("fast")
	c := newFake(t, testStore(), p)
	reconcileKey(t, newPolicyReconciler(c), "apps", "app")

	onlyJob(t, c, jobcfg.ModeBackup)
	v := onlyJob(t, c, jobcfg.ModeVerify)
	if v.Name != scheduledJobName("app", jobcfg.ModeVerify, time.Date(2026, 11, 22, 8, 30, 0, 0, time.UTC)) || v.Labels[jobcfg.LabelRole] != "verify" {
		t.Errorf("verify job = %+v", v.ObjectMeta)
	}
	pod := v.Spec.Template.Spec
	if !pod.Volumes[1].NFS.ReadOnly || !mount(pod.Containers[0], "store").ReadOnly {
		t.Error("verify store must be read-only")
	}
	scratch := volume(pod, "scratch")
	if scratch == nil || scratch.Ephemeral == nil || *scratch.Ephemeral.VolumeClaimTemplate.Spec.StorageClassName != "fast" ||
		mount(pod.Containers[0], "scratch").MountPath != "/scratch" {
		t.Errorf("scratch volume = %+v", scratch)
	}
	var cur v1alpha1.BackupPolicy
	mustGet(t, c, "apps", "app", &cur)
	if cur.Status.LastVerifyScheduleTime == nil {
		t.Error("lastVerifyScheduleTime not recorded")
	}

	// Without a storage class scratch is an emptyDir; without verify no verify Job is ever created.
	q := policyDue(postgresPolicy())
	q.Name = "plain"
	q.Spec.Verify = &v1alpha1.Verify{Schedule: "30 * * * *"}
	n := policyDue(postgresPolicy())
	n.Name = "noverify"
	c2 := newFake(t, testStore(), q, n)
	reconcileKey(t, newPolicyReconciler(c2), "apps", "plain")
	reconcileKey(t, newPolicyReconciler(c2), "apps", "noverify")
	vs := jobsOf(t, c2, jobcfg.ModeVerify)
	if len(vs) != 1 || volume(vs[0].Spec.Template.Spec, "scratch").EmptyDir == nil {
		t.Errorf("verify jobs = %d, scratch should be emptyDir", len(vs))
	}
}

func TestSchedulingStartsOneRunPerSlot(t *testing.T) {
	c := newFake(t, testStore(), policyDue(filesPolicy()))
	r := newPolicyReconciler(c)
	reconcileKey(t, r, "apps", "app")
	reconcileKey(t, r, "apps", "app")
	onlyJob(t, c, jobcfg.ModeBackup) // the second reconcile must not add another

	var p v1alpha1.BackupPolicy
	mustGet(t, c, "apps", "app", &p)
	if p.Status.LastScheduleTime == nil || !p.Status.LastScheduleTime.Time.Equal(testNow.Truncate(time.Second)) {
		t.Errorf("lastScheduleTime = %v", p.Status.LastScheduleTime)
	}
}

func TestNothingRunsBeforeTheFirstSlot(t *testing.T) {
	c := newFake(t, testStore(), filesPolicy()) // created at the test clock, hourly: first slot 11:00
	res := reconcileKey(t, newPolicyReconciler(c), "apps", "app")
	if n := len(jobsOf(t, c, jobcfg.ModeBackup)); n != 0 {
		t.Fatalf("jobs = %d before the first slot", n)
	}
	if want := time.Date(2026, 11, 22, 11, 0, 0, 0, time.UTC).Sub(testNow) + time.Second; res.RequeueAfter != want {
		t.Errorf("requeueAfter = %v, want %v", res.RequeueAfter, want)
	}
}

func TestCatchUpAfterOutageRunsOnce(t *testing.T) {
	p := filesPolicy()
	p.CreationTimestamp = metav1.NewTime(testNow.Add(-72 * time.Hour)) // 72 missed hourly slots
	c := newFake(t, testStore(), p)
	r := newPolicyReconciler(c)
	reconcileKey(t, r, "apps", "app")
	reconcileKey(t, r, "apps", "app")
	onlyJob(t, c, jobcfg.ModeBackup)
}

func TestNoSecondRunWhileOneIsActive(t *testing.T) {
	active := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "bs-app-run-1", Namespace: "apps", Labels: jobLabels("app", jobcfg.ModeBackup)}}
	c := newFake(t, testStore(), policyDue(filesPolicy()), active)
	reconcileKey(t, newPolicyReconciler(c), "apps", "app")
	if got := onlyJob(t, c, jobcfg.ModeBackup); got.Name != "bs-app-run-1" {
		t.Errorf("a second run was started: %s", got.Name)
	}
}

func TestSuspendedPolicyStartsNothing(t *testing.T) {
	p := policyDue(filesPolicy())
	p.Spec.Suspend = true
	c := newFake(t, testStore(), p)
	reconcileKey(t, newPolicyReconciler(c), "apps", "app")
	if n := len(jobsOf(t, c, jobcfg.ModeBackup)); n != 0 {
		t.Fatalf("jobs = %d for a suspended policy", n)
	}
	var got v1alpha1.BackupPolicy
	mustGet(t, c, "apps", "app", &got)
	if got.Status.NextSyncTime != nil {
		t.Error("suspended policy has no next sync")
	}
	if cond := meta.FindStatusCondition(got.Status.Conditions, "Ready"); cond == nil || cond.Reason != "Suspended" {
		t.Errorf("ready = %+v", cond)
	}
}

// A backup that fires while a restore is rewriting the volume would snapshot a half-restored tree.
func TestRestorePausesScheduling(t *testing.T) {
	rs := &v1alpha1.Restore{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "apps"}, Spec: v1alpha1.RestoreSpec{PolicyRef: "app"}}
	c := newFake(t, testStore(), policyDue(filesPolicy()), rs)
	r := newPolicyReconciler(c)
	reconcileKey(t, r, "apps", "app")
	if n := len(jobsOf(t, c, jobcfg.ModeBackup)); n != 0 {
		t.Fatalf("jobs = %d while a restore is active", n)
	}
	var got v1alpha1.BackupPolicy
	mustGet(t, c, "apps", "app", &got)
	if cond := meta.FindStatusCondition(got.Status.Conditions, "Ready"); cond == nil || cond.Reason != "RestoreInProgress" {
		t.Errorf("ready = %+v", cond)
	}

	mustGet(t, c, "apps", "r1", rs)
	rs.Status.Phase = phaseSucceeded
	if err := c.Status().Update(context.Background(), rs); err != nil {
		t.Fatal(err)
	}
	reconcileKey(t, r, "apps", "app")
	onlyJob(t, c, jobcfg.ModeBackup)
}

func TestManualTriggerCreatesExactlyOneJob(t *testing.T) {
	p := filesPolicy()
	p.Spec.Trigger.Manual = "now-1"
	c := newFake(t, testStore(), p)
	r := newPolicyReconciler(c)
	reconcileKey(t, r, "apps", "app")
	reconcileKey(t, r, "apps", "app")

	job := *onlyJob(t, c, jobcfg.ModeBackup)
	if job.Name != manualJobName("app", "now-1", 0) || job.Annotations["backshots.io/manual"] != "now-1" ||
		metav1.GetControllerOf(&job).Name != "app" {
		t.Errorf("manual job = %+v", job.ObjectMeta)
	}
	if job.Spec.Template.Spec.Containers[0].Name != "mover" {
		t.Error("manual job must run the mover container")
	}

	// Success of that Job records lastManualSync and stops further triggering.
	done, _ := finishedJob(job.Name, "app", jobcfg.ModeBackup, true, testNow, nil)
	job.Status = done.Status
	if err := c.Status().Update(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	reconcileKey(t, r, "apps", "app")
	var cur v1alpha1.BackupPolicy
	mustGet(t, c, "apps", "app", &cur)
	if cur.Status.LastManualSync != "now-1" {
		t.Errorf("lastManualSync = %q", cur.Status.LastManualSync)
	}
	onlyJob(t, c, jobcfg.ModeBackup)
}

func TestManualTriggerWaitsForARunningBackup(t *testing.T) {
	p := filesPolicy()
	p.Spec.Trigger.Manual = "now-1"
	active := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "bs-app-run-1", Namespace: "apps", Labels: jobLabels("app", jobcfg.ModeBackup)}}
	c := newFake(t, testStore(), p, active)
	reconcileKey(t, newPolicyReconciler(c), "apps", "app")
	if got := onlyJob(t, c, jobcfg.ModeBackup); got.Name != "bs-app-run-1" {
		t.Errorf("manual job started while a backup runs: %s", got.Name)
	}
}

func TestHistoryIsPruned(t *testing.T) {
	objs := []client.Object{testStore(), filesPolicy()}
	for i := 1; i <= 5; i++ {
		at := testNow.Add(-time.Duration(10-i) * time.Minute)
		ok, _ := finishedJob(fmt.Sprintf("bs-app-ok-%d", i), "app", jobcfg.ModeBackup, true, at, nil)
		bad, _ := finishedJob(fmt.Sprintf("bs-app-bad-%d", i), "app", jobcfg.ModeBackup, false, at, nil)
		objs = append(objs, ok, bad)
	}
	c := newFake(t, objs...)
	reconcileKey(t, newPolicyReconciler(c), "apps", "app")

	got := map[string]bool{}
	for _, j := range jobsOf(t, c, jobcfg.ModeBackup) {
		got[j.Name] = true
	}
	want := []string{"bs-app-ok-3", "bs-app-ok-4", "bs-app-ok-5", "bs-app-bad-3", "bs-app-bad-4", "bs-app-bad-5"}
	if len(got) != len(want) {
		t.Fatalf("kept %d jobs: %v", len(got), got)
	}
	for _, n := range want {
		if !got[n] {
			t.Errorf("newest job %s was pruned: %v", n, got)
		}
	}
}

func TestRequeueIn(t *testing.T) {
	p := filesPolicy()
	if got, want := requeueIn(p, testNow), time.Date(2026, 11, 22, 11, 0, 0, 0, time.UTC).Sub(testNow)+time.Second; got != want {
		t.Errorf("hourly: %v, want %v", got, want)
	}
	p.Spec.Suspend = true
	if got := requeueIn(p, testNow); got != 0 {
		t.Errorf("suspended: %v, want 0", got)
	}
}

func TestStoreNotFoundAndInvalidSpec(t *testing.T) {
	c := newFake(t, filesPolicy())
	r := newPolicyReconciler(c)
	reconcileKey(t, r, "apps", "app")
	var p v1alpha1.BackupPolicy
	mustGet(t, c, "apps", "app", &p)
	if cond := meta.FindStatusCondition(p.Status.Conditions, "Ready"); cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "StoreNotFound" {
		t.Errorf("ready = %+v", cond)
	}

	bad := filesPolicy()
	bad.Name = "bad"
	bad.Spec.Source.Files = nil // type says files, sub-struct missing
	if err := c.Create(context.Background(), bad); err != nil {
		t.Fatal(err)
	}
	reconcileKey(t, r, "apps", "bad")
	mustGet(t, c, "apps", "bad", &p)
	if cond := meta.FindStatusCondition(p.Status.Conditions, "Ready"); cond == nil || cond.Reason != "InvalidSpec" {
		t.Errorf("ready = %+v", cond)
	}
}

func TestStatusFromJobs(t *testing.T) {
	p := filesPolicy()
	p.Spec.Verify = &v1alpha1.Verify{Schedule: "0 4 * * *"}
	okAt := testNow.Add(-2 * time.Hour)
	failAt := testNow.Add(-time.Hour)
	okJob, okPod := finishedJob("bs-app-1", "app", jobcfg.ModeBackup, true, okAt, &jobcfg.Result{
		Mode: jobcfg.ModeBackup, OK: true, Snapshot: "2026-11-22T08-00-00Z", Time: okAt, Duration: 42.5, Bytes: 1000, Files: 7, Kept: 5,
	})
	failJob, failPod := finishedJob("bs-app-2", "app", jobcfg.ModeBackup, false, failAt, &jobcfg.Result{
		Mode: jobcfg.ModeBackup, Message: "source looks empty", Time: failAt,
	})
	verJob, verPod := finishedJob("bs-verify-app-1", "app", jobcfg.ModeVerify, true, failAt, &jobcfg.Result{
		Mode: jobcfg.ModeVerify, OK: true, Snapshot: "2026-11-22T08-00-00Z", Message: "3 assertions passed", Time: failAt,
	})
	c := newFake(t, testStore(), p, okJob, okPod, failJob, failPod, verJob, verPod)
	reconcileKey(t, newPolicyReconciler(c), "apps", "app")

	var got v1alpha1.BackupPolicy
	mustGet(t, c, "apps", "app", &got)
	st := got.Status
	if st.LastSnapshot == nil || st.LastSnapshot.Name != "2026-11-22T08-00-00Z" || st.LastSnapshot.Bytes != 1000 || st.LastSnapshot.Files != 7 {
		t.Errorf("lastSnapshot = %+v", st.LastSnapshot)
	}
	if st.LastSyncTime == nil || !st.LastSyncTime.Time.Equal(okAt) {
		t.Errorf("lastSyncTime = %v", st.LastSyncTime)
	}
	if st.LastSyncStartTime == nil || !st.LastSyncStartTime.Time.Equal(okAt.Add(-time.Minute)) {
		t.Errorf("lastSyncStartTime = %v", st.LastSyncStartTime)
	}
	if st.LastSyncDuration == nil || st.LastSyncDuration.Duration != 42500*time.Millisecond || st.SnapshotsKept != 5 {
		t.Errorf("duration/kept = %v/%d", st.LastSyncDuration, st.SnapshotsKept)
	}
	if st.LastVerify == nil || !st.LastVerify.OK || st.LastVerify.Message != "3 assertions passed" {
		t.Errorf("lastVerify = %+v", st.LastVerify)
	}
	if st.NextSyncTime == nil || !st.NextSyncTime.Time.Equal(time.Date(2026, 11, 22, 11, 0, 0, 0, time.UTC)) {
		t.Errorf("nextSyncTime = %v", st.NextSyncTime)
	}
	healthy := meta.FindStatusCondition(st.Conditions, "Healthy")
	if healthy == nil || healthy.Status != metav1.ConditionFalse || healthy.Message != "source looks empty" {
		t.Errorf("healthy = %+v", healthy)
	}
	if ready := meta.FindStatusCondition(st.Conditions, "Ready"); ready == nil || ready.Status != metav1.ConditionTrue {
		t.Errorf("ready = %+v", ready)
	}
}

func TestStatusUnknownBeforeFirstRunAndStable(t *testing.T) {
	c := newFake(t, testStore(), filesPolicy())
	r := newPolicyReconciler(c)
	reconcileKey(t, r, "apps", "app")
	var first v1alpha1.BackupPolicy
	mustGet(t, c, "apps", "app", &first)
	if h := meta.FindStatusCondition(first.Status.Conditions, "Healthy"); h == nil || h.Status != metav1.ConditionUnknown {
		t.Errorf("healthy = %+v", h)
	}
	reconcileKey(t, r, "apps", "app")
	var second v1alpha1.BackupPolicy
	mustGet(t, c, "apps", "app", &second)
	if first.ResourceVersion != second.ResourceVersion {
		t.Error("unchanged status must not be rewritten")
	}
}

func TestFailedJobWithoutTerminationMessage(t *testing.T) {
	job, _ := finishedJob("bs-app-1", "app", jobcfg.ModeBackup, false, testNow, nil)
	c := newFake(t, testStore(), filesPolicy(), job)
	reconcileKey(t, newPolicyReconciler(c), "apps", "app")
	var got v1alpha1.BackupPolicy
	mustGet(t, c, "apps", "app", &got)
	if h := meta.FindStatusCondition(got.Status.Conditions, "Healthy"); h == nil || h.Status != metav1.ConditionFalse || h.Message != "Done: job message" {
		t.Errorf("healthy = %+v", h)
	}
}

func TestVerifyNeedsDatabaseWhenAmbiguous(t *testing.T) {
	p := postgresPolicy()
	p.Spec.Verify = &v1alpha1.Verify{Schedule: "@daily", SQL: []string{"select true"}}
	if err := validatePolicy(p); err == nil {
		t.Error("verify.sql with no database list and no verify.database must be rejected")
	}
	p.Spec.Verify.Database = "app"
	if err := validatePolicy(p); err != nil {
		t.Errorf("explicit verify.database: %v", err)
	}
	p.Spec.Verify.Database = ""
	p.Spec.Source.Postgres.Databases = []string{"app"}
	if err := validatePolicy(p); err != nil {
		t.Errorf("single listed database: %v", err)
	}
}

func TestStaleness(t *testing.T) {
	last := time.Date(2026, 1, 1, 10, 30, 0, 0, time.UTC)
	mk := func() *v1alpha1.BackupPolicy {
		p := filesPolicy()
		p.Spec.Schedule = "@hourly" // due 11:00, grace a quarter hour
		p.Status.LastSyncTime = metaTime(last)
		meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
			Type: condHealthy, Status: metav1.ConditionTrue, Reason: "BackupSucceeded", Message: "latest backup succeeded"})
		return p
	}
	healthy := func(p *v1alpha1.BackupPolicy) *metav1.Condition {
		return meta.FindStatusCondition(p.Status.Conditions, condHealthy)
	}

	// Within the grace period (due 11:00 + 15m): still healthy.
	p := mk()
	deriveStatus(p, nil, nil, time.Date(2026, 1, 1, 11, 10, 0, 0, time.UTC))
	if c := healthy(p); c.Status != metav1.ConditionTrue {
		t.Fatalf("inside grace: %+v", c)
	}
	// Overdue with nothing running or finished (e.g. NFS down, pod stuck): Stale.
	p = mk()
	deriveStatus(p, nil, nil, time.Date(2026, 1, 1, 11, 20, 0, 0, time.UTC))
	if c := healthy(p); c.Status != metav1.ConditionFalse || c.Reason != "Stale" {
		t.Fatalf("overdue: %+v", c)
	}
	// Deriving again must not change anything, or the status update would re-trigger reconciles forever.
	before := p.Status.DeepCopy()
	deriveStatus(p, nil, nil, time.Date(2026, 1, 1, 11, 21, 0, 0, time.UTC))
	if !equality.Semantic.DeepEqual(before.Conditions, p.Status.Conditions) {
		t.Fatalf("condition churned between reconciles:\n%+v\n%+v", before.Conditions, p.Status.Conditions)
	}
	// Suspended policies are never stale.
	p = mk()
	p.Spec.Suspend = true
	deriveStatus(p, nil, nil, time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC))
	if c := healthy(p); c.Status != metav1.ConditionTrue {
		t.Fatalf("suspended: %+v", c)
	}
	// A recorded failure is more specific than Stale and must be kept.
	p = mk()
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: condHealthy, Status: metav1.ConditionFalse, Reason: "BackupFailed", Message: "rsync died"})
	deriveStatus(p, nil, nil, time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC))
	if c := healthy(p); c.Reason != "BackupFailed" || c.Message != "rsync died" {
		t.Fatalf("failure masked: %+v", c)
	}
}

func TestStaleAtGraceIsClamped(t *testing.T) {
	last := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) // a Thursday
	tests := []struct {
		schedule string
		want     time.Duration // stale moment, measured from the last success
	}{
		{"* * * * *", time.Minute + 5*time.Minute}, // due in 1m; a quarter of 1m is under the 5m floor
		{"@hourly", time.Hour + 15*time.Minute},    // due in 1h; grace is a quarter of 1h
		{"@daily", 24*time.Hour + 6*time.Hour},     // due in 24h; a quarter is exactly the 6h cap
		{"@weekly", 3*24*time.Hour + 6*time.Hour},  // next Sunday is 3d away; a quarter of 7d is capped at 6h
	}
	for _, tt := range tests {
		p := filesPolicy()
		p.Spec.Schedule = tt.schedule
		p.Status.LastSyncTime = metaTime(last)
		at, ok := staleAt(p)
		if !ok {
			t.Fatalf("%s: not computed", tt.schedule)
		}
		if got := at.Sub(last); got != tt.want {
			t.Errorf("%s: stale %v after the last success, want %v", tt.schedule, got, tt.want)
		}
	}
}

func TestTimeZoneAppliesToTheSchedule(t *testing.T) {
	created := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	// 09:00 in New York is 14:00 UTC in November (EST, UTC-5).
	got, ok := nextSlot("0 9 * * *", "America/New_York", nil, created)
	if !ok || !got.Equal(time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC)) {
		t.Errorf("zoned slot = %v ok=%v", got, ok)
	}
	got, _ = nextSlot("0 9 * * *", "", nil, created)
	if !got.Equal(time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("UTC slot = %v", got)
	}
}

func TestTimeZoneValidation(t *testing.T) {
	p := filesPolicy()
	p.Spec.TimeZone = "Mars/Olympus_Mons"
	if err := validatePolicy(p); err == nil {
		t.Error("an unknown time zone must be rejected")
	}
	p.Spec.TimeZone = "America/New_York"
	if err := validatePolicy(p); err != nil {
		t.Errorf("valid zone: %v", err)
	}
	for _, prefix := range []string{"TZ=UTC 0 * * * *", "CRON_TZ=UTC 0 * * * *"} {
		q := filesPolicy()
		q.Spec.Schedule = prefix
		if err := validatePolicy(q); err == nil {
			t.Errorf("%q: a TZ prefix must be rejected in favour of spec.timeZone", prefix)
		}
	}
}

// Setting a trigger value that was used earlier (after a different one) must run again.
func TestManualTriggerValueCanBeReused(t *testing.T) {
	p := filesPolicy()
	p.Generation = 1
	p.Spec.Trigger.Manual = "go"
	c := newFake(t, testStore(), p)
	r := newPolicyReconciler(c)
	reconcileKey(t, r, "apps", "app")
	first := *onlyJob(t, c, jobcfg.ModeBackup)
	done, _ := finishedJob(first.Name, "app", jobcfg.ModeBackup, true, testNow, nil)
	first.Status = done.Status
	if err := c.Status().Update(context.Background(), &first); err != nil {
		t.Fatal(err)
	}
	reconcileKey(t, r, "apps", "app")
	var cur v1alpha1.BackupPolicy
	mustGet(t, c, "apps", "app", &cur)
	if cur.Status.LastManualSync != "go" {
		t.Fatalf("lastManualSync = %q", cur.Status.LastManualSync)
	}

	// Another value, then back to the first one: a new generation each time. In this order: ranging over a map
	// would shuffle it and make the test pass or fail by luck.
	for _, step := range []struct {
		gen   int64
		value string
	}{{2, "other"}, {3, "go"}} {
		mustGet(t, c, "apps", "app", &cur)
		cur.Generation, cur.Spec.Trigger.Manual = step.gen, step.value
		if err := c.Update(context.Background(), &cur); err != nil {
			t.Fatal(err)
		}
		reconcileKey(t, r, "apps", "app")
		for _, j := range jobsOf(t, c, jobcfg.ModeBackup) {
			if ok, _ := jobState(&j); !ok {
				j := j
				d, _ := finishedJob(j.Name, "app", jobcfg.ModeBackup, true, testNow, nil)
				j.Status = d.Status
				if err := c.Status().Update(context.Background(), &j); err != nil {
					t.Fatal(err)
				}
			}
		}
		reconcileKey(t, r, "apps", "app")
	}
	if got := len(jobsOf(t, c, jobcfg.ModeBackup)); got != 3 {
		t.Errorf("manual jobs = %d, want 3 (go, other, go again)", got)
	}
}

func TestInstallContainerAndPodAreHardened(t *testing.T) {
	p := policyDue(filesPolicy())
	p.Spec.Pod.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "regcred"}}
	c := newFake(t, testStore(), p)
	reconcileKey(t, newPolicyReconciler(c), "apps", "app")
	pod := onlyJob(t, c, jobcfg.ModeBackup).Spec.Template.Spec

	init := pod.InitContainers[0]
	sc := init.SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.RunAsUser == nil || *sc.RunAsUser == 0 ||
		*sc.AllowPrivilegeEscalation || !*sc.ReadOnlyRootFilesystem || sc.Capabilities == nil || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("install container must be non-root, read-only, no capabilities: %+v", sc)
	}
	if init.Resources.Limits.Memory().IsZero() || init.Resources.Requests.Cpu().IsZero() {
		t.Errorf("install container needs requests and limits for quota'd namespaces: %+v", init.Resources)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("movers must not get a ServiceAccount token")
	}
	if len(pod.ImagePullSecrets) != 1 || pod.ImagePullSecrets[0].Name != "regcred" {
		t.Errorf("imagePullSecrets = %+v", pod.ImagePullSecrets)
	}
}

func TestHealthGaugeExistsForAPolicyThatNeverSucceeded(t *testing.T) {
	p := filesPolicy()
	p.Namespace, p.Name = "gauge-ns", "never"
	recordMetrics(p) // no Healthy condition yet: Unknown, so no series
	count := func() int {
		n, err := testutil.GatherAndCount(metrics.Registry, "backshots_policy_healthy")
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{Type: condHealthy, Status: metav1.ConditionFalse, Reason: "Stale", Message: "never ran"})
	recordMetrics(p) // stale and never succeeded: no last_success series, but the health series must exist
	if count() != before+1 {
		t.Error("backshots_policy_healthy must exist for an unhealthy policy that has never succeeded")
	}
	forgetMetrics("gauge-ns", "never")
}

func nextEvent(t *testing.T, rec *record.FakeRecorder) string {
	t.Helper()
	select {
	case e := <-rec.Events:
		return e
	default:
		return ""
	}
}

func TestHealthEventsOnTransitionsOnly(t *testing.T) {
	p := filesPolicy()
	rec := record.NewFakeRecorder(10)
	cond := func(s metav1.ConditionStatus, reason, msg string) *metav1.Condition {
		return &metav1.Condition{Type: condHealthy, Status: s, Reason: reason, Message: msg}
	}
	set := func(c *metav1.Condition) { meta.SetStatusCondition(&p.Status.Conditions, *c) }

	// Healthy -> Stale: one Warning.
	before := cond(metav1.ConditionTrue, "BackupSucceeded", "ok")
	set(cond(metav1.ConditionFalse, "Stale", "no successful backup since 10:00"))
	healthEvents(rec, p, before)
	if got := nextEvent(t, rec); !strings.HasPrefix(got, "Warning Stale ") {
		t.Fatalf("event = %q", got)
	}
	// Stale -> Stale: steady state, nothing.
	healthEvents(rec, p, cond(metav1.ConditionFalse, "Stale", "x"))
	if got := nextEvent(t, rec); got != "" {
		t.Errorf("a steady unhealthy state must not repeat the event: %q", got)
	}
	// Stale -> BackupFailed: the reason changed, say so.
	set(cond(metav1.ConditionFalse, "BackupFailed", "rsync died"))
	healthEvents(rec, p, cond(metav1.ConditionFalse, "Stale", "x"))
	if got := nextEvent(t, rec); !strings.HasPrefix(got, "Warning BackupFailed ") {
		t.Errorf("event = %q", got)
	}
	// BackupFailed -> healthy: a Normal Recovered event.
	set(cond(metav1.ConditionTrue, "BackupSucceeded", "latest backup succeeded"))
	healthEvents(rec, p, cond(metav1.ConditionFalse, "BackupFailed", "rsync died"))
	if got := nextEvent(t, rec); !strings.HasPrefix(got, "Normal Recovered ") {
		t.Errorf("event = %q", got)
	}
	// Unknown -> healthy (the first success) is not news.
	healthEvents(rec, p, cond(metav1.ConditionUnknown, "NoRuns", "x"))
	if got := nextEvent(t, rec); got != "" {
		t.Errorf("first success must not emit: %q", got)
	}
	healthEvents(nil, p, nil) // a nil recorder must be harmless
}

func TestRestoreEmitsPhaseEvents(t *testing.T) {
	c := newFake(t, testStore(), filesPolicy(), testRestore(func(r *v1alpha1.Restore) { r.Spec.ScaleDown = nil }))
	r := newRestoreReconciler(c)
	rec := record.NewFakeRecorder(20)
	r.Recorder = rec
	for range 3 {
		reconcileKey(t, r, "apps", "r1")
	}
	finishRestoreJob(t, c, true, jobcfg.Result{Mode: jobcfg.ModeRestore, OK: true, Snapshot: "2026-11-22T08-00-00Z"})
	reconcileKey(t, r, "apps", "r1")
	var got []string
	for e := nextEvent(t, rec); e != ""; e = nextEvent(t, rec) {
		got = append(got, e)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{"Normal Started ", "Normal Succeeded restored snapshot 2026-11-22T08-00-00Z"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing event %q in:\n%s", want, joined)
		}
	}
}
