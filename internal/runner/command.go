package runner

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	clientsetscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// captureCommand runs the policy's command in a running app pod and streams its stdout, gzipped, to
// stage/command/<file> — the same staging contract sqlite and postgres payloads follow, so the restic
// commit path is shared. The mover talks to the API with the scoped backshots-executor identity the
// controller maintains for the policy (pods get/list and pods/exec create, nothing else).
func (r *runner) captureCommand(ctx context.Context, stage string) error {
	cs := r.cfg.Spec.Source.Command
	if cs == nil {
		return fmt.Errorf("source.command is not set")
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	pod, err := pickExecPod(ctx, clientset, r.cfg.Namespace, cs.PodSelector, cs.Container)
	if err != nil {
		return err
	}
	r.logf("command source: running in pod %s container %q", pod, cs.Container)

	execIn := func(ctx context.Context, cmd []string, stdout io.Writer, stderr *bytes.Buffer) error {
		return podExec(ctx, cfg, clientset, r.cfg.Namespace, pod, cs.Container, cmd, stdout, stderr)
	}

	// A quiesce must be undone no matter how the dump ends, including a canceled or expired run
	// context: the app must not stay locked because the backup died.
	if len(cs.PostCommand) > 0 {
		defer func() {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
			defer cancel()
			var e bytes.Buffer
			if err := execIn(cctx, cs.PostCommand, nil, &e); err != nil {
				r.logf("WARNING: postCommand failed in %s: %v %s", pod, err, tail(e.String(), 300))
			}
		}()
	}
	if len(cs.PreCommand) > 0 {
		var e bytes.Buffer
		if err := execIn(ctx, cs.PreCommand, nil, &e); err != nil {
			return fmt.Errorf("preCommand: %w %s", err, tail(e.String(), 300))
		}
	}

	dir := filepath.Join(stage, "command")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, cs.File)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }() // content is durably written by the Sync at the end
	var stderr bytes.Buffer
	zw := gzip.NewWriter(f)
	count := &countingWriter{w: zw}
	if err := execIn(ctx, cs.Command, count, &stderr); err != nil {
		return fmt.Errorf("dump command: %w %s", err, tail(stderr.String(), 500))
	}
	if count.n == 0 {
		return fmt.Errorf("dump command produced no output")
	}
	if err := zw.Close(); err != nil {
		return err
	}
	r.logf("captured %s (%d bytes raw) from %s", cs.File, count.n, pod)
	return f.Sync()
}

// countingWriter counts the bytes that pass through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// pickExecPod chooses the oldest Running, ready pod whose labels match selector and that has the
// named container (any container if empty). It refuses to guess between candidates that differ in
// age only within the same second unless their names order them, and errors when nothing matches.
func pickExecPod(ctx context.Context, cs kubernetes.Interface, namespace string, selector map[string]string, container string) (string, error) {
	pods, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set(selector).String(),
		FieldSelector: "status.phase=Running",
	})
	if err != nil {
		return "", fmt.Errorf("listing pods: %w", err)
	}
	var ready []corev1.Pod
	for _, p := range pods.Items {
		if !podReady(p) || !hasContainer(p, container) {
			continue
		}
		ready = append(ready, p)
	}
	if len(ready) == 0 {
		return "", fmt.Errorf("no running, ready pod matches selector %v with container %q", selector, container)
	}
	sort.SliceStable(ready, func(i, j int) bool {
		if !ready[i].CreationTimestamp.Equal(&ready[j].CreationTimestamp) {
			return ready[i].CreationTimestamp.Before(&ready[j].CreationTimestamp)
		}
		return ready[i].Name < ready[j].Name
	})
	if len(ready) > 1 {
		names := make([]string, len(ready))
		for i := range ready {
			names[i] = ready[i].Name
		}
		return ready[0].Name, fmt.Errorf("selector %v matches %d running pods (%s): the command must run in exactly one; narrow podSelector",
			selector, len(ready), joinNames(names))
	}
	return ready[0].Name, nil
}

func joinNames(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}

func podReady(p corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func hasContainer(p corev1.Pod, name string) bool {
	if name == "" {
		return len(p.Spec.Containers) > 0
	}
	for _, c := range p.Spec.Containers {
		if c.Name == name {
			return true
		}
	}
	return false
}

// podExec runs cmd in the container over the exec API (SPDY), streaming stdout and stderr. A
// non-zero exit surfaces as an error carrying the remote exit status.
func podExec(ctx context.Context, cfg *rest.Config, clientset kubernetes.Interface, namespace, pod, container string, cmd []string, stdout io.Writer, stderr io.Writer) error {
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(namespace).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdout:    stdout != nil,
			Stderr:    stderr != nil,
		}, clientsetscheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
	if err != nil {
		return err
	}
	return executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: stdout, Stderr: stderr})
}
