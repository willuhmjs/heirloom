package controller

import (
	"encoding/json"
	"sort"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/willuhmjs/backshots/internal/jobcfg"
)

const labelJobName = "job-name"

// run is one finished mover Job together with its Result.
type run struct {
	job    *batchv1.Job
	ok     bool
	at     time.Time
	result jobcfg.Result
}

// jobState reports whether the Job finished and whether it succeeded.
func jobState(job *batchv1.Job) (done, succeeded bool) {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return true, true
		case batchv1.JobFailed:
			return true, false
		}
	}
	return false, false
}

func finishTime(job *batchv1.Job) time.Time {
	if t := job.Status.CompletionTime; t != nil {
		return t.Time
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return c.LastTransitionTime.Time
		}
	}
	return job.CreationTimestamp.Time
}

func failureMessage(job *batchv1.Job) string {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			if c.Message != "" {
				return c.Reason + ": " + c.Message
			}
			return c.Reason
		}
	}
	return "job failed"
}

// resultFor extracts the runner's Result from the termination message of the Job's "mover" container.
// Without a readable message (deadline exceeded, pod evicted) the outcome is synthesized from the Job.
func resultFor(job *batchv1.Job, pods []corev1.Pod) jobcfg.Result {
	_, ok := jobState(job)
	if res, found := terminationResult(job.Name, pods); found {
		return res
	}
	res := jobcfg.Result{OK: ok, Time: finishTime(job)}
	if !ok {
		res.Message = failureMessage(job)
	}
	return res
}

func terminationResult(jobName string, pods []corev1.Pod) (jobcfg.Result, bool) {
	var best *corev1.ContainerStateTerminated
	var bestRes jobcfg.Result
	for i := range pods {
		if pods[i].Labels[labelJobName] != jobName {
			continue
		}
		for _, cs := range pods[i].Status.ContainerStatuses {
			t := cs.State.Terminated
			if cs.Name != jobcfg.ContainerMover || t == nil || t.Message == "" {
				continue
			}
			var res jobcfg.Result
			if json.Unmarshal([]byte(t.Message), &res) != nil {
				continue
			}
			if best == nil || t.FinishedAt.After(best.FinishedAt.Time) {
				best, bestRes = t, res
			}
		}
	}
	return bestRes, best != nil
}

// finishedRuns returns the finished Jobs of one role, oldest first.
func finishedRuns(jobs []batchv1.Job, pods []corev1.Pod, role jobcfg.Mode) []run {
	var runs []run
	for i := range jobs {
		job := &jobs[i]
		if job.Labels[jobcfg.LabelRole] != string(role) {
			continue
		}
		done, ok := jobState(job)
		if !done {
			continue
		}
		res := resultFor(job, pods)
		res.OK = ok
		runs = append(runs, run{job: job, ok: ok, at: finishTime(job), result: res})
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].at.Before(runs[j].at) })
	return runs
}
