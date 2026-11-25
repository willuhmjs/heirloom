package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/willuhmjs/heirloom/api/v1"
	"github.com/willuhmjs/heirloom/internal/jobcfg"
)

const testImage = "ghcr.io/willuhmjs/heirloom:test"

var testNow = time.Date(2026, 11, 22, 10, 0, 30, 0, time.UTC)

func newFake(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	// The PVCs the test policies point at exist, as they would in a real cluster (a restore checks for them).
	for _, name := range []string{"app-data", "db"} {
		objs = append(objs, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "apps"}})
	}
	return fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&v1.BackupPolicy{}, &v1.Restore{}).
		WithObjects(objs...).Build()
}

func testStore() *v1.BackupStore {
	return &v1.BackupStore{
		ObjectMeta: metav1.ObjectMeta{Name: "nas"},
		Spec:       v1.BackupStoreSpec{NFS: v1.NFSSpec{Server: "10.0.0.2", Path: "/volume1/k8s"}},
	}
}

func filesPolicy() *v1.BackupPolicy {
	return &v1.BackupPolicy{
		// Created at the test clock: nothing is due until a test moves the creation time back (policyDue).
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "apps", UID: "uid-1", CreationTimestamp: metav1.NewTime(testNow)},
		Spec: v1.BackupPolicySpec{
			StoreRef: "nas",
			Schedule: "0 * * * *",
			Source:   v1.Source{Type: v1.SourceFiles, Files: &v1.FilesSource{PVC: "app-data"}},
		},
	}
}

func sqlitePolicy() *v1.BackupPolicy {
	p := filesPolicy()
	p.Spec.Source = v1.Source{Type: v1.SourceSQLite, SQLite: &v1.SQLiteSource{PVC: "db", Paths: []string{"a.db"}}}
	return p
}

func postgresPolicy() *v1.BackupPolicy {
	p := filesPolicy()
	p.Spec.Source = v1.Source{Type: v1.SourcePostgres, Postgres: &v1.PostgresSource{
		Image: "postgres:16", Host: "pg.db", Port: 5432, User: "postgres",
		PasswordSecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "pg"}, Key: "password"},
	}}
	return p
}

func newPolicyReconciler(c client.Client) *PolicyReconciler {
	return &PolicyReconciler{Client: c, Reader: c, Scheme: c.Scheme(), Image: testImage, Now: func() time.Time { return testNow }}
}

func reconcileKey(t *testing.T, r interface {
	Reconcile(context.Context, ctrl.Request) (ctrl.Result, error)
}, ns, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: ns, Name: name}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func mustGet(t *testing.T, c client.Client, ns, name string, obj client.Object) {
	t.Helper()
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, obj); err != nil {
		t.Fatalf("get %s/%s: %v", ns, name, err)
	}
}

// finishedJob returns a finished mover Job plus its Pod carrying res as termination message.
func finishedJob(name, policy string, role jobcfg.Mode, ok bool, finished time.Time, res *jobcfg.Result) (*batchv1.Job, *corev1.Pod) {
	labels := jobLabels(policy, role)
	cond := batchv1.JobComplete
	if !ok {
		cond = batchv1.JobFailed
	}
	ft := metav1.NewTime(finished)
	st := metav1.NewTime(finished.Add(-time.Minute))
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "apps", Labels: labels},
		Status: batchv1.JobStatus{
			StartTime:  &st,
			Conditions: []batchv1.JobCondition{{Type: cond, Status: corev1.ConditionTrue, LastTransitionTime: ft, Reason: "Done", Message: "job message"}},
		},
	}
	if ok {
		job.Status.CompletionTime = &ft
	}
	if res == nil {
		return job, nil
	}
	msg, _ := json.Marshal(res)
	podLabels := jobLabels(policy, role)
	podLabels[labelJobName] = name
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-abc", Namespace: "apps", Labels: podLabels},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  jobcfg.ContainerMover,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: string(msg), FinishedAt: ft}},
		}}},
	}
	return job, pod
}
