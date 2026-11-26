package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/willuhmjs/heirloom/api/v1"
	"github.com/willuhmjs/heirloom/internal/jobcfg"
)

// commandPolicy is a files policy whose source execs a dump command in the web pod.
func commandPolicy() *v1.BackupPolicy {
	p := filesPolicy()
	p.Spec.Source = v1.Source{Type: v1.SourceCommand, Command: &v1.CommandSource{
		PodSelector: map[string]string{"app": "web"},
		Command:     []string{"sh", "-c", "mysqldump --all-databases"},
		File:        "all-databases.sql.gz",
	}}
	return p
}

// --- jitter -----------------------------------------------------------------------

func TestJitterOffsetStableAndBounded(t *testing.T) {
	p := filesPolicy()
	if got := jitterOffset(p, jobcfg.ModeBackup); got != 0 {
		t.Errorf("unset jitter must offset by 0, got %s", got)
	}
	p.Spec.Jitter = "not-a-duration"
	if got := jitterOffset(p, jobcfg.ModeBackup); got != 0 {
		t.Errorf("unparsable jitter must offset by 0 (validatePolicy reports it), got %s", got)
	}
	p.Spec.Jitter = "10m"
	first := jitterOffset(p, jobcfg.ModeBackup)
	if first < 0 || first >= 10*time.Minute {
		t.Fatalf("offset %s outside [0, 10m)", first)
	}
	for range 5 {
		if got := jitterOffset(p, jobcfg.ModeBackup); got != first {
			t.Fatalf("offset must be the same every reconcile: %s then %s", first, got)
		}
	}
}

func TestJitterValidationAndSlotShift(t *testing.T) {
	last := metav1.NewTime(testNow.Add(-time.Hour)) // 09:00, so the next hour slot is 10:00
	base, ok := nextSlot("0 * * * *", "", &last, testNow, 0)
	if !ok || !base.Equal(testNow.Truncate(time.Hour)) {
		t.Fatalf("nextSlot = %s (ok=%v), want 10:00", base, ok)
	}
	shifted, _ := nextSlot("0 * * * *", "", &last, testNow, 7*time.Minute)
	if !shifted.Equal(base.Add(7 * time.Minute)) {
		t.Errorf("offset slot = %s, want %s", shifted, base.Add(7*time.Minute))
	}

	p := filesPolicy()
	p.Spec.Jitter = "5m"
	if err := validatePolicy(p); err != nil {
		t.Errorf("jitter 5m must be valid: %v", err)
	}
	p.Spec.Jitter = "7h"
	if err := validatePolicy(p); err == nil || !strings.Contains(err.Error(), "jitter") {
		t.Errorf("jitter above the 6h bound must be rejected, got %v", err)
	}
	p.Spec.Jitter = "soon"
	if err := validatePolicy(p); err == nil || !strings.Contains(err.Error(), "jitter") {
		t.Errorf("unparsable jitter must be rejected, got %v", err)
	}
}

func TestJitterMakesSlotDueOnlyAfterOffset(t *testing.T) {
	// A policy whose created-time slot is 10:00 plus its fixed jitter offset: no backup starts before
	// that instant and exactly one starts after it.
	p := policyDue(filesPolicy())
	p.CreationTimestamp = metav1.NewTime(testNow.Add(-10 * time.Minute))
	p.Spec.Schedule = "0 * * * *"
	p.Spec.Jitter = "20m"
	slot := testNow.Truncate(time.Hour).Add(jitterOffset(p, jobcfg.ModeBackup)) // 10:00 + the policy's offset

	c := newFake(t, testStore(), p)
	r := newPolicyReconciler(c)
	r.Now = func() time.Time { return slot.Add(-time.Minute) }
	reconcileKey(t, r, "apps", "app")
	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("the jittered slot must not start before its offset passed, started: %v", jobNames(jobs.Items))
	}

	r.Now = func() time.Time { return slot.Add(time.Minute) }
	reconcileKey(t, r, "apps", "app")
	onlyJob(t, c, jobcfg.ModeBackup) // exactly one backup exists now
}

func jobNames(jobs []batchv1.Job) []string {
	var out []string
	for _, j := range jobs {
		out = append(out, j.Name)
	}
	return out
}

// --- repository mutex: restore waits for movers -----------------------------------

func TestRestoreWaitsForActiveMover(t *testing.T) {
	active := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "bs-app-backup-x", Namespace: "apps", Labels: jobLabels("app", jobcfg.ModeBackup)},
		Status:     batchv1.JobStatus{StartTime: &metav1.Time{Time: testNow.Add(-time.Minute)}, Active: 1},
	}
	c := newFake(t, testStore(), filesPolicy(), testDeployment(nil),
		testRestore(func(r *v1.Restore) { r.Spec.ScaleDown = nil }), active)
	r := newRestoreReconciler(c)

	reconcileKey(t, r, "apps", "r1") // "" -> Pending
	reconcileKey(t, r, "apps", "r1") // Pending: must hold while the backup runs
	rs := getRestore(t, c)
	if rs.Status.Phase != phasePending {
		t.Fatalf("phase = %s, want Pending while a mover is active", rs.Status.Phase)
	}
	if !strings.Contains(rs.Status.Message, "bs-app-backup-x") {
		t.Errorf("message = %q, want it to name the mover being awaited", rs.Status.Message)
	}
	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("only the awaited backup may exist, got %v", jobNames(jobs.Items))
	}

	// The backup finishes; the next pass must move on without waiting for anything else.
	done, _ := finishedJob(active.Name, "app", jobcfg.ModeBackup, true, testNow, nil)
	active.Status = done.Status
	if err := c.Status().Update(context.Background(), active); err != nil {
		t.Fatal(err)
	}
	reconcileKey(t, r, "apps", "r1")
	if got := getRestore(t, c).Status.Phase; got == phasePending {
		t.Fatalf("restore still Pending after the mover finished")
	}
}

// --- HPA quiesce ------------------------------------------------------------------

func testHPA() *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "apps"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "web"},
			MinReplicas:    ptr.To[int32](2),
			MaxReplicas:    5,
		},
	}
}

func getHPA(t *testing.T, c client.Client) *autoscalingv2.HorizontalPodAutoscaler {
	t.Helper()
	var h autoscalingv2.HorizontalPodAutoscaler
	mustGet(t, c, "apps", "web", &h)
	return &h
}

// driveRestore reconciles until the restore reaches Running, simulating the workload stopping once
// the controller has scaled it down. Unlike driveToRunning it tolerates the extra status-persisting
// passes the HPA recording adds.
func driveRestore(t *testing.T, c client.Client, r *RestoreReconciler) {
	t.Helper()
	stopped := false
	for range 12 {
		rs := getRestore(t, c)
		if rs.Status.Phase == phaseRunning {
			reconcileKey(t, r, "apps", "r1") // Running -> creates the Job
			return
		}
		res := reconcileKey(t, r, "apps", "r1")
		if !stopped && rs.Status.Phase == phaseScalingDown && res.RequeueAfter > 0 {
			stopDeployment(t, c)
			stopped = true
		}
	}
	t.Fatalf("restore never reached Running (phase %s)", getRestore(t, c).Status.Phase)
}

func TestQuiesceHPAsParksAndRestoresBounds(t *testing.T) {
	c := newFake(t, testStore(), filesPolicy(), testDeployment(nil), testHPA(), testRestore(nil))
	r := newRestoreReconciler(c)

	driveRestore(t, c, r)
	if recs := getRestore(t, c).Status.ScaledHPAs; len(recs) != 1 || recs[0].Name != "web" || recs[0].MinReplicas != 2 || recs[0].MaxReplicas != 5 {
		t.Fatalf("status.scaledHPAs = %+v, want one record {web 2 5}", recs)
	}
	h := getHPA(t, c)
	if h.Spec.MaxReplicas != 0 || h.Spec.MinReplicas == nil || *h.Spec.MinReplicas != 0 {
		t.Errorf("autoscaler not parked: min=%v max=%d", h.Spec.MinReplicas, h.Spec.MaxReplicas)
	}

	finishRestoreJob(t, c, true, jobcfg.Result{Mode: jobcfg.ModeRestore, OK: true, Snapshot: "abc123"})
	for range 4 {
		reconcileKey(t, r, "apps", "r1")
		if getRestore(t, c).Status.Phase == phaseSucceeded {
			break
		}
	}
	h = getHPA(t, c)
	if h.Spec.MaxReplicas != 5 || h.Spec.MinReplicas == nil || *h.Spec.MinReplicas != 2 {
		t.Errorf("bounds not restored: min=%v max=%d, want 2/5", h.Spec.MinReplicas, h.Spec.MaxReplicas)
	}
	if got := getRestore(t, c).Status.Phase; got != phaseSucceeded {
		t.Errorf("phase = %s, want Succeeded", got)
	}
}

// An autoscaler aimed at something the restore does not scale down stays untouched.
func TestQuiesceHPAsLeavesOtherTargetsAlone(t *testing.T) {
	other := testHPA()
	other.Name = "api"
	other.Spec.ScaleTargetRef.Name = "api"
	c := newFake(t, testStore(), filesPolicy(), testDeployment(nil), other, testRestore(nil))
	r := newRestoreReconciler(c)

	driveRestore(t, c, r)
	var h autoscalingv2.HorizontalPodAutoscaler
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "apps", Name: "api"}, &h); err != nil {
		t.Fatal(err)
	}
	if h.Spec.MaxReplicas != 5 || *h.Spec.MinReplicas != 2 {
		t.Errorf("unrelated autoscaler touched: %+v", h.Spec)
	}
	if len(getRestore(t, c).Status.ScaledHPAs) != 0 {
		t.Errorf("unrelated autoscaler recorded: %+v", getRestore(t, c).Status.ScaledHPAs)
	}
}

// --- command source: validation, RBAC, pod identity --------------------------------

func TestRestoreOfCommandSourceNeedsTargetPVC(t *testing.T) {
	c := newFake(t, testStore(), commandPolicy(), testRestore(func(r *v1.Restore) {
		r.Spec.ScaleDown = nil
		r.Spec.Paths = nil
	}))
	r := newRestoreReconciler(c)
	reconcileKey(t, r, "apps", "r1")
	reconcileKey(t, r, "apps", "r1")
	rs := getRestore(t, c)
	if rs.Status.Phase != phaseFailed {
		t.Fatalf("phase = %s, want Failed", rs.Status.Phase)
	}
	if !strings.Contains(rs.Status.Message, "targetPVC") {
		t.Errorf("message = %q, want it to explain targetPVC is required", rs.Status.Message)
	}
}

func TestRestoreOfCommandSourceWritesToTargetPVC(t *testing.T) {
	c := newFake(t, testStore(), commandPolicy(), testRestore(func(r *v1.Restore) {
		r.Spec.ScaleDown = nil
		r.Spec.Paths = nil
		r.Spec.TargetPVC = "app-data"
	}))
	r := newRestoreReconciler(c)
	reconcileKey(t, r, "apps", "r1") // "" -> Pending
	reconcileKey(t, r, "apps", "r1") // Pending -> Running (no scaleDown)
	reconcileKey(t, r, "apps", "r1") // creates the Job
	job := onlyJob(t, c, jobcfg.ModeRestore)
	pod := job.Spec.Template.Spec
	if v := volume(pod, volSource); v == nil || v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != "app-data" || v.PersistentVolumeClaim.ReadOnly {
		t.Errorf("source volume = %+v, want a writable app-data claim", v)
	}
	// The restore writes dumps itself; it never execs, so it needs no executor identity.
	if pod.ServiceAccountName == ExecutorServiceAccount || ptr.Deref(pod.AutomountServiceAccountToken, false) {
		t.Errorf("restore pod carries the executor identity: sa=%q automount=%v", pod.ServiceAccountName, pod.AutomountServiceAccountToken)
	}
}

func TestExecutorRBACForCommandPolicies(t *testing.T) {
	other := commandPolicy()
	other.Name = "app2"
	other.UID = "uid-2"
	c := newFake(t, testStore(), policyDue(commandPolicy()), policyDue(other))
	r := newPolicyReconciler(c)
	reconcileKey(t, r, "apps", "app")
	reconcileKey(t, r, "apps", "app2")

	var sa corev1.ServiceAccount
	mustGet(t, c, "apps", ExecutorServiceAccount, &sa)
	if len(sa.OwnerReferences) != 2 {
		t.Errorf("service account owners = %+v, want both command policies", sa.OwnerReferences)
	}
	var role rbacv1.Role
	mustGet(t, c, "apps", ExecutorServiceAccount, &role)
	want := []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}},
		{APIGroups: []string{""}, Resources: []string{"pods/exec"}, Verbs: []string{"create"}},
	}
	if len(role.Rules) != len(want) {
		t.Errorf("role rules = %+v, want exactly pod lookup and pods/exec", role.Rules)
	}
	var rb rbacv1.RoleBinding
	mustGet(t, c, "apps", ExecutorServiceAccount, &rb)
	if rb.RoleRef.Name != ExecutorServiceAccount || len(rb.Subjects) != 1 || rb.Subjects[0].Name != ExecutorServiceAccount {
		t.Errorf("role binding = %+v", rb)
	}

	// The mover pod runs as that identity with a token.
	job := jobForPolicy(t, c, "app", jobcfg.ModeBackup)
	pod := job.Spec.Template.Spec
	if pod.ServiceAccountName != ExecutorServiceAccount || !ptr.Deref(pod.AutomountServiceAccountToken, false) {
		t.Errorf("command mover identity: sa=%q automount=%v", pod.ServiceAccountName, pod.AutomountServiceAccountToken)
	}
}

func jobForPolicy(t *testing.T, c client.Client, policy string, role jobcfg.Mode) *batchv1.Job {
	t.Helper()
	var list batchv1.JobList
	if err := c.List(context.Background(), &list, client.MatchingLabels{
		jobcfg.LabelPolicy: policy, jobcfg.LabelRole: string(role),
	}); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("%s/%s jobs = %d, want 1", policy, role, len(list.Items))
	}
	return &list.Items[0]
}

func TestNonCommandPoliciesGetNoExecutorRBAC(t *testing.T) {
	c := newFake(t, testStore(), policyDue(filesPolicy()))
	r := newPolicyReconciler(c)
	reconcileKey(t, r, "apps", "app")
	var sa corev1.ServiceAccount
	err := c.Get(context.Background(), client.ObjectKey{Namespace: "apps", Name: ExecutorServiceAccount}, &sa)
	if err == nil {
		t.Error("a files policy must not create the executor identity")
	}
	pod := onlyJob(t, c, jobcfg.ModeBackup).Spec.Template.Spec
	if pod.ServiceAccountName != "" || ptr.Deref(pod.AutomountServiceAccountToken, true) {
		t.Errorf("files mover carries an identity: sa=%q automount=%v", pod.ServiceAccountName, pod.AutomountServiceAccountToken)
	}
}

// --- store mountOptions: static PV/PVC provisioning --------------------------------

func nfsStore() *v1.BackupStore {
	s := testStore()
	s.Spec.NFS.MountOptions = []string{"nfsvers=4.1", "hard", "timeo=50"}
	return s
}

func getStorePV(t *testing.T, c client.Client, name string) *corev1.PersistentVolume {
	t.Helper()
	var pv corev1.PersistentVolume
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, &pv); err != nil {
		t.Fatalf("get pv %s: %v", name, err)
	}
	return &pv
}

func TestStoreClaimProvisionedForMountOptions(t *testing.T) {
	second := filesPolicy()
	second.Name = "app2"
	second.UID = "uid-2"
	c := newFake(t, nfsStore(), policyDue(filesPolicy()), policyDue(second))
	r := newPolicyReconciler(c)
	reconcileKey(t, r, "apps", "app")
	reconcileKey(t, r, "apps", "app2")

	name := storeClaimName("apps", "nas")
	pv := getStorePV(t, c, name)
	if pv.Spec.NFS == nil || pv.Spec.NFS.Server != "10.0.0.2" || pv.Spec.NFS.Path != "/volume1/k8s" {
		t.Errorf("pv nfs = %+v", pv.Spec.NFS)
	}
	if len(pv.Spec.MountOptions) != 3 || pv.Spec.MountOptions[0] != "nfsvers=4.1" {
		t.Errorf("pv mountOptions = %v", pv.Spec.MountOptions)
	}
	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
		t.Errorf("reclaim policy = %s: the PV is only a pointer at the export", pv.Spec.PersistentVolumeReclaimPolicy)
	}
	var pvc corev1.PersistentVolumeClaim
	mustGet(t, c, "apps", name, &pvc)
	if pvc.Spec.VolumeName != name || ptr.Deref(pvc.Spec.StorageClassName, "x") != "" {
		t.Errorf("pvc = %+v, want it bound to %s with no storage class", pvc.Spec, name)
	}
	if len(pvc.OwnerReferences) != 2 {
		t.Errorf("pvc owners = %+v, want both policies", pvc.OwnerReferences)
	}

	// Movers mount the claim instead of the inline export.
	pod := jobForPolicy(t, c, "app", jobcfg.ModeBackup).Spec.Template.Spec
	v := volume(pod, volStore)
	if v == nil || v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != name {
		t.Errorf("store volume = %+v, want claim %s", v, name)
	}

	// A released leftover is deleted so the next pass recreates it bound to the live claim.
	pv = getStorePV(t, c, name)
	pv.Status.Phase = corev1.VolumeReleased
	if err := c.Status().Update(context.Background(), pv); err != nil { // the fake keeps status off regular updates
		t.Fatal(err)
	}
	reconcileKey(t, r, "apps", "app")
	err := c.Get(context.Background(), client.ObjectKey{Name: name}, &corev1.PersistentVolume{})
	if err == nil {
		t.Error("released PV must be deleted for recreation")
	}
}

func TestStoreClaimMountOptionsDriftIsReported(t *testing.T) {
	store := nfsStore()
	c := newFake(t, store, policyDue(filesPolicy()))
	r := newPolicyReconciler(c)
	reconcileKey(t, r, "apps", "app")

	// The store now asks for different options; the bound PV cannot change them in place.
	store.Spec.NFS.MountOptions = []string{"nfsvers=4.2"}
	if err := c.Update(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	reconcileKey(t, r, "apps", "app")
	var p v1.BackupPolicy
	mustGet(t, c, "apps", "app", &p)
	cond := meta.FindStatusCondition(p.Status.Conditions, "Ready")
	if cond == nil || cond.Status != metav1.ConditionFalse || !strings.Contains(cond.Message, "kubectl delete pv") {
		t.Errorf("Ready = %+v, want an actionable immutability message", cond)
	}
}

func smbStore() *v1.BackupStore {
	return &v1.BackupStore{
		ObjectMeta: metav1.ObjectMeta{Name: "smb"},
		Spec: v1.BackupStoreSpec{SMB: &v1.SMBSpec{
			Server:               "10.0.0.3",
			Share:                "k8s-backup",
			CredentialsSecretRef: v1.SecretRef{Name: "smb-creds", Namespace: "heirloom-system"},
			MountOptions:         []string{"vers=3.0", "uid=0"},
		}},
	}
}

func TestSMBStoreAlwaysUsesStaticPV(t *testing.T) {
	p := policyDue(filesPolicy())
	p.Spec.StoreRef = "smb"
	c := newFake(t, smbStore(), p)
	r := newPolicyReconciler(c)
	reconcileKey(t, r, "apps", "app")

	name := storeClaimName("apps", "smb")
	pv := getStorePV(t, c, name)
	csi := pv.Spec.CSI
	if csi == nil || csi.Driver != "smb.csi.k8s.io" || csi.VolumeAttributes["source"] != "//10.0.0.3/k8s-backup" {
		t.Fatalf("pv csi = %+v", csi)
	}
	if ref := csi.NodeStageSecretRef; ref == nil || ref.Name != "smb-creds" || ref.Namespace != "heirloom-system" {
		t.Errorf("nodeStageSecretRef = %+v", ref)
	}
	if !strings.Contains(csi.VolumeHandle, "#"+name+"-") {
		t.Errorf("volumeHandle %q must be unique per PV", csi.VolumeHandle)
	}
	changed := smbStore()
	changed.Spec.SMB.MountOptions = append(changed.Spec.SMB.MountOptions, "nobrl")
	other, _ := storePVSource(changed, name)
	if other.CSI.VolumeHandle == csi.VolumeHandle {
		t.Error("new mount options must change the volumeHandle, or the node plugin reuses its old staged mount")
	}
	if pv.Spec.NFS != nil || len(pv.Spec.MountOptions) != 2 || pv.Spec.MountOptions[0] != "vers=3.0" {
		t.Errorf("pv = %+v", pv.Spec)
	}
	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		t.Errorf("reclaim policy = %s, want Retain (csi-driver-smb does not delete static volumes)", pv.Spec.PersistentVolumeReclaimPolicy)
	}
	pod := jobForPolicy(t, c, "app", jobcfg.ModeBackup).Spec.Template.Spec
	if v := volume(pod, volStore); v == nil || v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != name {
		t.Errorf("store volume = %+v, want claim %s", v, name)
	}
}

func TestSMBStoreWithoutMountOptionsStillUsesStaticPV(t *testing.T) {
	store := smbStore()
	store.Spec.SMB.MountOptions = nil
	p := policyDue(filesPolicy())
	p.Spec.StoreRef = "smb"
	c := newFake(t, store, p)
	reconcileKey(t, newPolicyReconciler(c), "apps", "app")
	if pv := getStorePV(t, c, storeClaimName("apps", "smb")); pv.Spec.CSI == nil {
		t.Errorf("pv = %+v, want a CSI volume", pv.Spec)
	}
}

func TestNoStoreClaimWithoutMountOptions(t *testing.T) {
	c := newFake(t, testStore(), policyDue(filesPolicy()))
	r := newPolicyReconciler(c)
	reconcileKey(t, r, "apps", "app")
	pod := onlyJob(t, c, jobcfg.ModeBackup).Spec.Template.Spec
	if v := volume(pod, volStore); v == nil || v.NFS == nil {
		t.Errorf("store volume = %+v, want the plain inline NFS mount as before", v)
	}
	var pvs corev1.PersistentVolumeList
	if err := c.List(context.Background(), &pvs); err != nil {
		t.Fatal(err)
	}
	if len(pvs.Items) != 0 {
		t.Errorf("no PVs may be provisioned without mountOptions, got %v", pvs.Items)
	}
}

// --- mover resource defaults --------------------------------------------------------

func TestMoverDefaultResourceRequests(t *testing.T) {
	c := newFake(t, testStore(), policyDue(filesPolicy()))
	reconcileKey(t, newPolicyReconciler(c), "apps", "app")
	res := onlyJob(t, c, jobcfg.ModeBackup).Spec.Template.Spec.Containers[0].Resources
	if res.Requests.Cpu().Cmp(resource.MustParse("100m")) != 0 || res.Requests.Memory().Cmp(resource.MustParse("512Mi")) != 0 {
		t.Errorf("default requests = %+v, want 100m/512Mi", res.Requests)
	}
	if len(res.Limits) != 0 {
		t.Errorf("limits must stay unset by default, got %+v", res.Limits)
	}

	p := policyDue(filesPolicy())
	p.Spec.Pod.Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}
	c = newFake(t, testStore(), p)
	reconcileKey(t, newPolicyReconciler(c), "apps", "app")
	res = onlyJob(t, c, jobcfg.ModeBackup).Spec.Template.Spec.Containers[0].Resources
	if res.Requests.Cpu().Cmp(resource.MustParse("2")) != 0 {
		t.Errorf("explicit cpu request must be kept, got %+v", res.Requests)
	}
	if res.Requests.Memory().Cmp(resource.MustParse("512Mi")) != 0 {
		t.Errorf("missing memory request must default to 512Mi, got %+v", res.Requests)
	}
}

// --- store encryption: per-policy derived repository passwords ----------------------

func encryptedStore() *v1.BackupStore {
	s := testStore()
	s.Spec.Encryption = &v1.EncryptionSpec{PasswordSecretRef: v1.SecretKeyRef{Name: "master", Namespace: "heirloom-system", Key: "password"}}
	return s
}

func masterSecret(pw string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "master", Namespace: "heirloom-system"},
		Data:       map[string][]byte{"password": []byte(pw)},
	}
}

func TestDerivePasswordIsStableAndPerPolicy(t *testing.T) {
	// openssl: printf 'heirloom/v1/apps/app' | openssl dgst -sha256 -hmac master -hex
	const want = "4b72ad8e56940d20c91f40c23aef6e093933842fd258d4b7a8b632b1178121a3"
	if got := DerivePassword([]byte("master"), "apps", "app"); got != want {
		t.Errorf("derive = %s, want %s (the runbook documents this exact derivation)", got, want)
	}
	if DerivePassword([]byte("master"), "apps", "app") == DerivePassword([]byte("master"), "apps", "app2") ||
		DerivePassword([]byte("master"), "apps", "app") == DerivePassword([]byte("master"), "other", "app") {
		t.Error("two policies share a repository password")
	}
}

func TestEncryptedStoreGetsDerivedKeySecretAndEnv(t *testing.T) {
	c := newFake(t, encryptedStore(), masterSecret("master"), policyDue(filesPolicy()))
	reconcileKey(t, newPolicyReconciler(c), "apps", "app")

	var sec corev1.Secret
	mustGet(t, c, "apps", repoKeySecretName("apps", "app"), &sec)
	if got := string(sec.Data["password"]) + sec.StringData["password"]; got != DerivePassword([]byte("master"), "apps", "app") {
		t.Errorf("derived secret = %q", got)
	}
	if len(sec.OwnerReferences) != 1 {
		t.Errorf("owners = %+v, want the policy so it is garbage-collected with it", sec.OwnerReferences)
	}
	job := jobForPolicy(t, c, "app", jobcfg.ModeBackup)
	var found bool
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name == jobcfg.EnvRepoPassword {
			found = true
			if e.Value != "" || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef.Name != repoKeySecretName("apps", "app") {
				t.Errorf("password env must be a secretKeyRef to the derived secret, never a literal: %+v", e)
			}
		}
		if e.Name == jobcfg.EnvConfig && !strings.Contains(e.Value, `"encrypted":true`) {
			t.Errorf("config does not mark the run encrypted: %s", e.Value)
		}
	}
	if !found {
		t.Error("mover has no RESTIC_PASSWORD")
	}
}

func TestEncryptedStoreWaitsForMasterSecret(t *testing.T) {
	c := newFake(t, encryptedStore(), policyDue(filesPolicy()))
	reconcileKey(t, newPolicyReconciler(c), "apps", "app")
	var p v1.BackupPolicy
	mustGet(t, c, "apps", "app", &p)
	cond := meta.FindStatusCondition(p.Status.Conditions, "Ready")
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "EncryptionKeyPending" {
		t.Errorf("Ready = %+v, want EncryptionKeyPending", cond)
	}
	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs); err != nil || len(jobs.Items) != 0 {
		t.Errorf("a run started without its password: %v %d", err, len(jobs.Items))
	}
}

func TestPlainStoreHasNoPasswordEnv(t *testing.T) {
	c := newFake(t, testStore(), policyDue(filesPolicy()))
	reconcileKey(t, newPolicyReconciler(c), "apps", "app")
	for _, e := range onlyJob(t, c, jobcfg.ModeBackup).Spec.Template.Spec.Containers[0].Env {
		if e.Name == jobcfg.EnvRepoPassword {
			t.Errorf("plain store mover carries %s", e.Name)
		}
	}
}

func TestStatusCarriesStoreSizeAndLastResult(t *testing.T) {
	c := newFake(t, encryptedStore(), masterSecret("master"), policyDue(filesPolicy()))
	reconcileKey(t, newPolicyReconciler(c), "apps", "app")
	var p v1.BackupPolicy
	mustGet(t, c, "apps", "app", &p)
	if p.Status.Store != "nas" || !p.Status.Encrypted {
		t.Errorf("store/encrypted = %q/%v, want nas/true", p.Status.Store, p.Status.Encrypted)
	}
	st := &v1.BackupPolicyStatus{}
	applySuccess(st, run{job: &batchv1.Job{}, at: testNow, ok: true, result: jobcfg.Result{Snapshot: "abc", Added: 2048, RepoBytes: 5 << 20, Kept: 3}})
	if st.LastAdded != "2.0 KiB" || st.RepoSize != "5.0 MiB" || st.SnapshotsKept != 3 {
		t.Errorf("status = %+v", st)
	}
}

func TestStoreStatusShowsTarget(t *testing.T) {
	for _, tc := range []struct {
		store *v1.BackupStore
		want  string
		enc   bool
	}{
		{testStore(), "nfs://10.0.0.2/volume1/k8s", false},
		{smbStore(), "smb://10.0.0.3/k8s-backup", false},
		{encryptedStore(), "nfs://10.0.0.2/volume1/k8s", true},
	} {
		c := newFake(t, tc.store)
		r := &StoreReconciler{Client: c}
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: tc.store.Name}}); err != nil {
			t.Fatal(err)
		}
		var got v1.BackupStore
		if err := c.Get(context.Background(), client.ObjectKey{Name: tc.store.Name}, &got); err != nil {
			t.Fatal(err)
		}
		if got.Status.Target != tc.want || got.Status.Encrypted != tc.enc {
			t.Errorf("status = %+v, want target %q encrypted %v", got.Status, tc.want, tc.enc)
		}
	}
}
