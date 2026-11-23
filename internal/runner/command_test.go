package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func execPod(name string, age time.Duration, ready bool, containers ...string) *corev1.Pod {
	if len(containers) == 0 {
		containers = []string{"app"}
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "ns",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
			Labels:            map[string]string{"app": "db"},
		},
		Spec: corev1.PodSpec{Containers: make([]corev1.Container, 0, len(containers))},
	}
	for _, c := range containers {
		pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: c})
	}
	if ready {
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	return pod
}

func TestPickExecPod(t *testing.T) {
	ctx := context.Background()
	sel := map[string]string{"app": "db"}

	_, err := pickExecPod(ctx, fake.NewSimpleClientset(), "ns", sel, "")
	if err == nil || !strings.Contains(err.Error(), "no running, ready pod") {
		t.Errorf("empty namespace: err = %v, want the no-match explanation", err)
	}

	cs := fake.NewSimpleClientset(execPod("db-2", time.Hour, true), execPod("db-1", 2*time.Hour, false))
	name, err := pickExecPod(ctx, cs, "ns", sel, "")
	if err != nil || name != "db-2" {
		t.Errorf("only the ready pod may be picked: name=%q err=%v", name, err)
	}

	cs = fake.NewSimpleClientset(execPod("db-1", 2*time.Hour, true), execPod("db-2", time.Hour, true))
	name, err = pickExecPod(ctx, cs, "ns", sel, "")
	if err == nil || !strings.Contains(err.Error(), "db-1, db-2") || name != "db-1" {
		t.Errorf("two matches must name both and refuse: name=%q err=%v", name, err)
	}

	// A container filter must exclude pods that do not have the named container.
	cs = fake.NewSimpleClientset(execPod("sidecar", 3*time.Hour, true, "logship"), execPod("db", time.Hour, true, "mysql", "logship"))
	name, err = pickExecPod(ctx, cs, "ns", sel, "mysql")
	if err != nil || name != "db" {
		t.Errorf("container filter: name=%q err=%v, want db", name, err)
	}

	// Pods of other namespaces never compete.
	other := execPod("elsewhere", time.Hour, true)
	other.Namespace = "other"
	cs = fake.NewSimpleClientset(execPod("db", time.Hour, true), other)
	name, err = pickExecPod(ctx, cs, "ns", sel, "")
	if err != nil || name != "db" {
		t.Errorf("namespace scoping: name=%q err=%v, want db", name, err)
	}
}
