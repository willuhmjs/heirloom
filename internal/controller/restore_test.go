package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/willuhmjs/backshots/api/v1alpha1"
	"github.com/willuhmjs/backshots/internal/jobcfg"
)

func testDeployment(annotations map[string]string) *appsv1.Deployment {
	labels := map[string]string{"app": "web"}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "apps", Annotations: annotations},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To[int32](3), Selector: &metav1.LabelSelector{MatchLabels: labels}},
		Status:     appsv1.DeploymentStatus{Replicas: 3},
	}
}

func testRestore(mutate func(*v1alpha1.Restore)) *v1alpha1.Restore {
	rs := &v1alpha1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "apps", UID: "uid-r"},
		Spec: v1alpha1.RestoreSpec{
			PolicyRef: "app",
			From:      "latest",
			Paths:     []string{"conf"},
			Delete:    true,
			ScaleDown: []v1alpha1.WorkloadRef{{Kind: "Deployment", Name: "web"}},
		},
	}
	if mutate != nil {
		mutate(rs)
	}
	return rs
}

func newRestoreReconciler(c client.Client) *RestoreReconciler {
	return &RestoreReconciler{Client: c, Reader: c, Scheme: c.Scheme(), Image: testImage, Now: func() time.Time { return testNow }}
}

func getRestore(t *testing.T, c client.Client) *v1alpha1.Restore {
	t.Helper()
	var rs v1alpha1.Restore
	mustGet(t, c, "apps", "r1", &rs)
	return &rs
}

func getDeployment(t *testing.T, c client.Client) *appsv1.Deployment {
	t.Helper()
	var d appsv1.Deployment
	mustGet(t, c, "apps", "web", &d)
	return &d
}

// stopDeployment simulates the Deployment controller finishing the scale-down.
func stopDeployment(t *testing.T, c client.Client) {
	t.Helper()
	d := getDeployment(t, c)
	d.Status.Replicas = 0
	if err := c.Status().Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
}

func finishRestoreJob(t *testing.T, c client.Client, ok bool, res jobcfg.Result) {
	t.Helper()
	var job batchv1.Job
	mustGet(t, c, "apps", "bs-restore-r1", &job)
	done, pod := finishedJob(job.Name, "app", jobcfg.ModeRestore, ok, testNow, &res)
	job.Status = done.Status
	if err := c.Status().Update(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
}

// driveToRunning reconciles until the restore Job exists.
func driveToRunning(t *testing.T, c client.Client, r *RestoreReconciler) {
	t.Helper()
	reconcileKey(t, r, "apps", "r1") // "" -> Pending
	reconcileKey(t, r, "apps", "r1") // Pending -> ScalingDown
	if got := getRestore(t, c).Status.Phase; got != phaseScalingDown {
		t.Fatalf("phase = %s, want ScalingDown", got)
	}
	reconcileKey(t, r, "apps", "r1") // records, scales down, waits
	res := reconcileKey(t, r, "apps", "r1")
	if res.RequeueAfter == 0 || getRestore(t, c).Status.Phase != phaseScalingDown {
		t.Fatalf("must wait for pods to stop: %+v phase=%s", res, getRestore(t, c).Status.Phase)
	}
	stopDeployment(t, c)
	reconcileKey(t, r, "apps", "r1") // -> Running
	reconcileKey(t, r, "apps", "r1") // creates Job
}

func TestRestoreHappyPath(t *testing.T) {
	c := newFake(t, testStore(), filesPolicy(), testDeployment(nil), testRestore(nil))
	r := newRestoreReconciler(c)
	driveToRunning(t, c, r)

	d := getDeployment(t, c)
	if *d.Spec.Replicas != 0 || d.Annotations[annotationFluxReconcile] != "disabled" {
		t.Errorf("scale down not applied: replicas=%d ann=%v", *d.Spec.Replicas, d.Annotations)
	}
	if sc := getRestore(t, c).Status.Scaled; len(sc) != 1 || sc[0].Replicas != 3 || sc[0].Name != "web" {
		t.Errorf("scaled = %+v", sc)
	}

	var job batchv1.Job
	mustGet(t, c, "apps", "bs-restore-r1", &job)
	pod := job.Spec.Template.Spec
	if *job.Spec.BackoffLimit != 0 || job.Labels[jobcfg.LabelRole] != "restore" || metav1.GetControllerOf(&job).Name != "r1" {
		t.Errorf("job = %+v", job.ObjectMeta)
	}
	if !pod.Volumes[1].NFS.ReadOnly || pod.Volumes[2].PersistentVolumeClaim.ReadOnly || pod.Volumes[2].PersistentVolumeClaim.ClaimName != "app-data" {
		t.Errorf("volumes = %+v", pod.Volumes)
	}
	cfg := pod.Containers[0].Env[0].Value
	for _, want := range []string{`"mode":"restore"`, `"from":"latest"`, `"paths":["conf"]`, `"delete":true`} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config missing %s: %s", want, cfg)
		}
	}

	finishRestoreJob(t, c, true, jobcfg.Result{Mode: jobcfg.ModeRestore, OK: true, Snapshot: "2026-11-22T08-00-00Z", Time: testNow})
	reconcileKey(t, r, "apps", "r1")
	rs := getRestore(t, c)
	if rs.Status.Phase != phaseScalingUp || rs.Status.Snapshot != "2026-11-22T08-00-00Z" {
		t.Fatalf("status = %+v", rs.Status)
	}
	reconcileKey(t, r, "apps", "r1")
	rs = getRestore(t, c)
	d = getDeployment(t, c)
	if rs.Status.Phase != phaseSucceeded || rs.Status.CompletionTime == nil {
		t.Errorf("status = %+v", rs.Status)
	}
	if *d.Spec.Replicas != 3 || len(d.Annotations) != 0 {
		t.Errorf("scale up not applied: replicas=%d ann=%v", *d.Spec.Replicas, d.Annotations)
	}

	// A finished Restore is never touched again.
	d.Spec.Replicas = ptr.To[int32](0)
	if err := c.Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	reconcileKey(t, r, "apps", "r1")
	if *getDeployment(t, c).Spec.Replicas != 0 {
		t.Error("finished restore must not scale again")
	}
}

func TestRestoreKeepsPreexistingFluxAnnotation(t *testing.T) {
	pre := map[string]string{annotationFluxReconcile: "disabled"}
	c := newFake(t, testStore(), filesPolicy(), testDeployment(pre), testRestore(nil))
	r := newRestoreReconciler(c)
	driveToRunning(t, c, r)
	if _, marked := getDeployment(t, c).Annotations[annotationRestoredBy]; marked {
		t.Error("must not claim an annotation it did not add")
	}
	finishRestoreJob(t, c, true, jobcfg.Result{Mode: jobcfg.ModeRestore, OK: true, Snapshot: "s"})
	reconcileKey(t, r, "apps", "r1")
	reconcileKey(t, r, "apps", "r1")
	d := getDeployment(t, c)
	if d.Annotations[annotationFluxReconcile] != "disabled" || *d.Spec.Replicas != 3 {
		t.Errorf("replicas=%d ann=%v", *d.Spec.Replicas, d.Annotations)
	}
}

func TestRestoreFailureLeavesWorkloadDownByDefault(t *testing.T) {
	c := newFake(t, testStore(), filesPolicy(), testDeployment(nil), testRestore(nil))
	r := newRestoreReconciler(c)
	driveToRunning(t, c, r)
	finishRestoreJob(t, c, false, jobcfg.Result{Mode: jobcfg.ModeRestore, Message: "snapshot not found"})
	reconcileKey(t, r, "apps", "r1")
	rs := getRestore(t, c)
	if rs.Status.Phase != phaseFailed || rs.Status.Message != "snapshot not found" {
		t.Errorf("status = %+v", rs.Status)
	}
	if d := getDeployment(t, c); *d.Spec.Replicas != 0 || d.Annotations[annotationFluxReconcile] != "disabled" {
		t.Errorf("workload must stay down: %d %v", *d.Spec.Replicas, d.Annotations)
	}
}

func TestRestoreFailureScalesUpWhenRequested(t *testing.T) {
	rs := testRestore(func(r *v1alpha1.Restore) { r.Spec.ScaleUpOnFailure = true })
	c := newFake(t, testStore(), filesPolicy(), testDeployment(nil), rs)
	r := newRestoreReconciler(c)
	driveToRunning(t, c, r)
	finishRestoreJob(t, c, false, jobcfg.Result{Mode: jobcfg.ModeRestore, Message: "rsync failed"})
	reconcileKey(t, r, "apps", "r1")
	if got := getRestore(t, c).Status.Phase; got != phaseScalingUp {
		t.Fatalf("phase = %s", got)
	}
	reconcileKey(t, r, "apps", "r1")
	got := getRestore(t, c)
	d := getDeployment(t, c)
	if got.Status.Phase != phaseFailed || got.Status.Message != "rsync failed" {
		t.Errorf("status = %+v", got.Status)
	}
	if *d.Spec.Replicas != 3 || len(d.Annotations) != 0 {
		t.Errorf("replicas=%d ann=%v", *d.Spec.Replicas, d.Annotations)
	}
}

func TestRestoreWithoutScaleDownSkipsScaling(t *testing.T) {
	rs := testRestore(func(r *v1alpha1.Restore) { r.Spec.ScaleDown = nil })
	c := newFake(t, testStore(), sqlitePolicy(), rs)
	r := newRestoreReconciler(c)
	reconcileKey(t, r, "apps", "r1")
	reconcileKey(t, r, "apps", "r1") // Pending; paths are invalid for sqlite
	got := getRestore(t, c)
	if got.Status.Phase != phaseFailed || !strings.Contains(got.Status.Message, "sqlite") {
		t.Fatalf("status = %+v", got.Status)
	}

	valid := testRestore(func(r *v1alpha1.Restore) {
		r.Name, r.Spec.ScaleDown, r.Spec.Paths, r.Spec.Delete, r.Spec.TargetPVC = "r2", nil, nil, false, "scratch-pvc"
	})
	scratch := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "scratch-pvc", Namespace: "apps"}}
	if err := c.Create(context.Background(), scratch); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(context.Background(), valid); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		reconcileKey(t, r, "apps", "r2")
	}
	var job batchv1.Job
	mustGet(t, c, "apps", "bs-restore-r2", &job)
	if vol := volume(job.Spec.Template.Spec, "source"); vol.PersistentVolumeClaim.ClaimName != "scratch-pvc" || vol.PersistentVolumeClaim.ReadOnly {
		t.Errorf("source volume = %+v", vol)
	}
}

func TestRestorePostgresJob(t *testing.T) {
	rs := testRestore(func(r *v1alpha1.Restore) {
		r.Spec.ScaleDown, r.Spec.Paths, r.Spec.Delete = nil, nil, false
		r.Spec.Databases = []string{"app"}
	})
	c := newFake(t, testStore(), postgresPolicy(), rs)
	r := newRestoreReconciler(c)
	for range 3 {
		reconcileKey(t, r, "apps", "r1")
	}
	var job batchv1.Job
	mustGet(t, c, "apps", "bs-restore-r1", &job)
	mover := job.Spec.Template.Spec.Containers[0]
	if mover.Image != "postgres:16" || mover.Env[1].Name != "PGPASSWORD" || volume(job.Spec.Template.Spec, "source") != nil {
		t.Errorf("mover = %+v", mover)
	}
}

func TestRestoreMissingPolicyFails(t *testing.T) {
	c := newFake(t, testStore(), testRestore(nil))
	r := newRestoreReconciler(c)
	reconcileKey(t, r, "apps", "r1")
	reconcileKey(t, r, "apps", "r1")
	got := getRestore(t, c)
	if got.Status.Phase != phaseFailed || !strings.Contains(got.Status.Message, "not found") {
		t.Errorf("status = %+v", got.Status)
	}
}

func TestWaitsForPodsOfStoppedWorkload(t *testing.T) {
	c := newFake(t, testStore(), filesPolicy(), testDeployment(nil), testRestore(nil),
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "apps", Labels: map[string]string{"app": "web"}}})
	r := newRestoreReconciler(c)
	driveToRunningUntilStopped(t, c, r)
	if res := reconcileKey(t, r, "apps", "r1"); res.RequeueAfter == 0 {
		t.Error("must keep waiting while a pod remains")
	}
	if err := c.Delete(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "apps"}}); err != nil {
		t.Fatal(err)
	}
	reconcileKey(t, r, "apps", "r1")
	if got := getRestore(t, c).Status.Phase; got != phaseRunning {
		t.Errorf("phase = %s", got)
	}
}

func driveToRunningUntilStopped(t *testing.T, c client.Client, r *RestoreReconciler) {
	t.Helper()
	for range 3 {
		reconcileKey(t, r, "apps", "r1")
	}
	stopDeployment(t, c)
}

// Typos are caught before any workload is stopped.
func TestRestoreRejectsBadInputsBeforeScalingDown(t *testing.T) {
	cases := map[string]func(*v1alpha1.Restore){
		"from":        func(r *v1alpha1.Restore) { r.Spec.From = "2026-11-22" },
		"path":        func(r *v1alpha1.Restore) { r.Spec.Paths = []string{"a/../../etc"} },
		"database":    func(r *v1alpha1.Restore) { r.Spec.Databases = []string{"a/b"} },
		"pvc":         func(r *v1alpha1.Restore) { r.Spec.TargetPVC = "does-not-exist" },
		"name length": func(r *v1alpha1.Restore) { r.Name = strings.Repeat("x", maxRestoreNameLen+1) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			rs := testRestore(mutate)
			if name == "database" {
				rs.Spec.Paths = nil
			}
			c := newFake(t, testStore(), filesPolicy(), rs, testDeployment(nil))
			r := newRestoreReconciler(c)
			for range 3 {
				reconcileKey(t, r, "apps", rs.Name)
			}
			var got v1alpha1.Restore
			mustGet(t, c, "apps", rs.Name, &got)
			if got.Status.Phase != phaseFailed || got.Status.Message == "" {
				t.Fatalf("status = %+v", got.Status)
			}
			if d := getDeployment(t, c); *d.Spec.Replicas != 3 {
				t.Errorf("workload was scaled to %d before the input was rejected", *d.Spec.Replicas)
			}
		})
	}
}

// Workloads that never stop must not leave the restore, or the app, stuck.
func TestRestoreScaleDownTimeoutBringsWorkloadsBack(t *testing.T) {
	c := newFake(t, testStore(), filesPolicy(), testRestore(nil), testDeployment(nil))
	r := newRestoreReconciler(c)
	for range 4 { // Pending, ScalingDown, record+scale down, wait
		reconcileKey(t, r, "apps", "r1")
	}
	if got := getRestore(t, c).Status.Phase; got != phaseScalingDown {
		t.Fatalf("phase = %s, want ScalingDown", got)
	}
	r.Now = func() time.Time { return testNow.Add(scaleDownTimeout + time.Minute) }
	for range 3 {
		reconcileKey(t, r, "apps", "r1")
	}
	got := getRestore(t, c)
	if got.Status.Phase != phaseFailed || !strings.Contains(got.Status.Message, "did not stop") {
		t.Fatalf("status = %+v", got.Status)
	}
	if d := getDeployment(t, c); *d.Spec.Replicas != 3 {
		t.Errorf("replicas = %d: the restore never ran, so the app must come back (scaleUpOnFailure is false)", *d.Spec.Replicas)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "apps", Name: "bs-restore-r1"}, &batchv1.Job{}); err == nil {
		t.Error("a restore Job was started although the workload never stopped")
	}
}

// Deleting a Restore mid-flight must not strand the workloads it scaled down.
func TestDeletingARestoreScalesWorkloadsBackUp(t *testing.T) {
	c := newFake(t, testStore(), filesPolicy(), testRestore(nil), testDeployment(nil))
	r := newRestoreReconciler(c)
	driveToRunning(t, c, r)
	if d := getDeployment(t, c); *d.Spec.Replicas != 0 {
		t.Fatalf("setup: replicas = %d", *d.Spec.Replicas)
	}
	rs := getRestore(t, c)
	if !controllerutil.ContainsFinalizer(rs, finalizerName) {
		t.Fatal("restore with scaleDown must carry the finalizer")
	}
	if err := c.Delete(context.Background(), rs); err != nil {
		t.Fatal(err)
	}
	reconcileKey(t, r, "apps", "r1")
	if d := getDeployment(t, c); *d.Spec.Replicas != 3 {
		t.Errorf("replicas = %d after delete, want 3", *d.Spec.Replicas)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "apps", Name: "r1"}, &v1alpha1.Restore{}); !apierrors.IsNotFound(err) {
		t.Errorf("restore should be gone once the finalizer ran: %v", err)
	}
}

// Two restores of one policy run one after the other, oldest first.
func TestRestoresOfOnePolicyQueue(t *testing.T) {
	older := testRestore(func(r *v1alpha1.Restore) {
		r.Spec.ScaleDown = nil
		r.CreationTimestamp = metav1.NewTime(testNow.Add(-time.Minute))
	})
	newer := testRestore(func(r *v1alpha1.Restore) {
		r.Name, r.Spec.ScaleDown = "r2", nil
		r.CreationTimestamp = metav1.NewTime(testNow)
	})
	c := newFake(t, testStore(), filesPolicy(), older, newer)
	r := newRestoreReconciler(c)
	reconcileKey(t, r, "apps", "r2") // "" -> Pending
	res := reconcileKey(t, r, "apps", "r2")
	var got v1alpha1.Restore
	mustGet(t, c, "apps", "r2", &got)
	if got.Status.Phase != phasePending || !strings.Contains(got.Status.Message, `waiting for restore "r1"`) || res.RequeueAfter == 0 {
		t.Fatalf("the newer restore must wait: %+v %+v", got.Status, res)
	}
	reconcileKey(t, r, "apps", "r1")
	reconcileKey(t, r, "apps", "r1")
	if got := getRestore(t, c).Status.Phase; got != phaseRunning {
		t.Fatalf("the older restore must go first, phase = %s", got)
	}
	reconcileKey(t, r, "apps", "r2")
	mustGet(t, c, "apps", "r2", &got)
	if got.Status.Phase != phasePending {
		t.Errorf("r2 must keep waiting while r1 runs, phase = %s", got.Status.Phase)
	}
}
