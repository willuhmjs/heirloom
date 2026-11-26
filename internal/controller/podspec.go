package controller

import (
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	v1 "github.com/willuhmjs/heirloom/api/v1"
	"github.com/willuhmjs/heirloom/internal/jobcfg"
	"github.com/willuhmjs/heirloom/internal/snapshot"
)

const (
	volStore   = "store"
	volSource  = "source"
	volScratch = "scratch"
	volTools   = "tools"

	defaultActiveDeadline int64 = 3600
	defaultScratchSize          = "10Gi"

	// ExecutorServiceAccount is the namespace-local identity created for policies whose source execs
	// into app pods (command sources). Its Role grants nothing but pod lookup and pods/exec.
	ExecutorServiceAccount = "heirloom-executor"
)

// moverPod holds everything needed to render the pod of one backup, verify or restore run,
// following the contract documented in package jobcfg.
type moverPod struct {
	mode        jobcfg.Mode
	namespace   string
	policy      string
	spec        v1.BackupPolicySpec
	store       *v1.BackupStore
	runnerImage string
	restore     *jobcfg.Restore
	// sourcePVC overrides the policy's PVC (restore into another claim).
	sourcePVC string
}

// The store is always mounted writable, even for verify and restore: restic writes repository locks
// even for pure read commands, so a read-only mount makes `restic snapshots` retry a lock Save
// forever. The isolation boundary is a mover only ever mounting its own policy folder of the export.

// needsScratch: every mover run mounts the scratch volume. The runner points restic's chunk cache
// (--cache-dir) at it so a large cache never lands on the node's root disk; restores and verifies of
// sqlite/postgres payloads are materialized through it, and the throwaway postgres for verify.sql
// lives there. pod.scratchStorageClassName sizes it.
func (m moverPod) needsScratch() bool { return true }

func (m moverPod) readOnlySource() bool {
	return m.spec.Source.Type == v1.SourceFiles && m.mode != jobcfg.ModeRestore
}

func (m moverPod) moverImage() string {
	if m.spec.Source.Type == v1.SourcePostgres {
		return m.spec.Source.Postgres.Image
	}
	return m.runnerImage
}

func (m moverPod) claim() string {
	if m.sourcePVC != "" {
		return m.sourcePVC
	}
	switch m.spec.Source.Type {
	case v1.SourceFiles:
		return m.spec.Source.Files.PVC
	case v1.SourceSQLite:
		return m.spec.Source.SQLite.PVC
	}
	return ""
}

// needsExecutorRBAC reports whether mover pods of this policy need the in-cluster identity that lets
// them exec into app pods (command sources; restore of a command source writes to a PVC and does not
// exec).
func (m moverPod) needsExecutorRBAC() bool {
	return m.spec.Source.Type == v1.SourceCommand && m.mode == jobcfg.ModeBackup
}

func (m moverPod) podSpec() (corev1.PodSpec, error) {
	if err := validateSource(m.spec.Source); err != nil {
		return corev1.PodSpec{}, err
	}
	cfg := jobcfg.Config{
		Mode:      m.mode,
		Namespace: m.namespace,
		Policy:    m.policy,
		Spec:      m.spec,
		Retention: resolveRetention(m.spec.Retention, m.store),
		Restore:   m.restore,
	}
	encoded, err := cfg.Encode()
	if err != nil {
		return corev1.PodSpec{}, fmt.Errorf("encoding runner config: %w", err)
	}
	spec := corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		// Movers never talk to the Kubernetes API; do not hand them a token. The one exception is the
		// command source: its mover execs into the app pod through the API using the scoped
		// heirloom-executor identity the controller maintains for the policy.
		AutomountServiceAccountToken: ptr.To(m.needsExecutorRBAC()),
		ImagePullSecrets:             m.spec.Pod.ImagePullSecrets,
		InitContainers:               []corev1.Container{m.installContainer()},
		Containers:                   []corev1.Container{m.moverContainer(encoded)},
		Volumes:                      m.volumes(),
		NodeSelector:                 m.spec.Pod.NodeSelector,
		Tolerations:                  m.spec.Pod.Tolerations,
		PriorityClassName:            m.spec.Pod.PriorityClassName,
	}
	if m.needsExecutorRBAC() {
		spec.ServiceAccountName = ExecutorServiceAccount
	}
	return spec, nil
}

func (m moverPod) installContainer() corev1.Container {
	return corev1.Container{
		Name:         jobcfg.ContainerInstall,
		Image:        m.runnerImage,
		Command:      []string{jobcfg.ImageBinary, "install", jobcfg.ToolsBinary},
		VolumeMounts: []corev1.VolumeMount{{Name: volTools, MountPath: jobcfg.ToolsMount}},
		// It only copies one file into an emptyDir: no reason for root, capabilities or a writable root filesystem.
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             ptr.To(true),
			RunAsUser:                ptr.To[int64](65532),
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		// Namespaces with a ResourceQuota that demands requests and limits would otherwise reject the pod.
		// The limit has room because the container is itself a large static binary (the operator's, and now
		// restic's too, both mmapped and both counted against the cgroup), not because the copy needs it.
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("192Mi")},
		},
	}
}

func (m moverPod) moverContainer(encodedConfig string) corev1.Container {
	env := []corev1.EnvVar{{Name: jobcfg.EnvConfig, Value: encodedConfig}}
	if pg := m.spec.Source.Postgres; pg != nil {
		env = append(env, corev1.EnvVar{
			Name:      "PGPASSWORD",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: pg.PasswordSecretRef.DeepCopy()},
		})
	}
	mounts := []corev1.VolumeMount{
		{Name: volTools, MountPath: jobcfg.ToolsMount, ReadOnly: true},
		// Scoped to this policy's directory: the user-chosen postgres image and symlinks planted in a PVC
		// must not be able to reach other namespaces' backups.
		{
			Name: volStore, MountPath: jobcfg.StoreMount,
			SubPath: jobcfg.StoreSubPath(m.namespace, m.policy),
		},
	}
	if m.claim() != "" {
		mounts = append(mounts, corev1.VolumeMount{Name: volSource, MountPath: jobcfg.SourceMount, ReadOnly: m.readOnlySource()})
	}
	if m.needsScratch() {
		mounts = append(mounts, corev1.VolumeMount{Name: volScratch, MountPath: jobcfg.ScratchMount})
	}
	return corev1.Container{
		Name:    jobcfg.ContainerMover,
		Image:   m.moverImage(),
		Command: []string{jobcfg.ToolsBinary, "run"},
		Env:     env,
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                ptr.To[int64](0),
			AllowPrivilegeEscalation: ptr.To(false),
			// Root, but only with what restic (open the whole store, chowns restored files) and the postgres privilege drop need.
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
				Add:  []corev1.Capability{"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL", "SETUID", "SETGID"},
			},
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		TerminationMessagePath:   jobcfg.TerminationLog,
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		Resources:                m.moverResources(),
		VolumeMounts:             mounts,
	}
}

// moverResources are the pod's resources with defaults applied where unset. A best-effort mover is
// the first pod a node evicts under disk or memory pressure, and restic's repository index alone runs
// a few hundred MiB, so the default requests size for that. Limits stay open unless the user sets
// them: a limit that is too low turns into a self-inflicted backup failure on a large repository.
func (m moverPod) moverResources() corev1.ResourceRequirements {
	res := *m.spec.Pod.Resources.DeepCopy()
	if res.Requests == nil {
		res.Requests = corev1.ResourceList{}
	}
	if _, ok := res.Requests[corev1.ResourceCPU]; !ok {
		res.Requests[corev1.ResourceCPU] = resource.MustParse("100m")
	}
	if _, ok := res.Requests[corev1.ResourceMemory]; !ok {
		res.Requests[corev1.ResourceMemory] = resource.MustParse("512Mi")
	}
	return res
}

func (m moverPod) volumes() []corev1.Volume {
	// With mountOptions, or for an SMB share, the store arrives through a provisioned static PV/PVC (an inline
	// NFS volume cannot carry mount options); otherwise the mover mounts the export directly.
	storeVol := corev1.Volume{Name: volStore}
	if storeNeedsClaim(m.store) {
		storeVol.VolumeSource = corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: storeClaimName(m.namespace, m.store.Name),
		}}
	} else {
		storeVol.VolumeSource = corev1.VolumeSource{NFS: &corev1.NFSVolumeSource{
			Server: m.store.Spec.NFS.Server,
			Path:   m.store.Spec.NFS.Path,
		}}
	}
	vols := []corev1.Volume{
		{Name: volTools, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		storeVol,
	}
	if c := m.claim(); c != "" {
		vols = append(vols, corev1.Volume{Name: volSource, VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: c, ReadOnly: m.readOnlySource()},
		}})
	}
	if m.needsScratch() {
		vols = append(vols, corev1.Volume{Name: volScratch, VolumeSource: m.scratchSource()})
	}
	return vols
}

// scratchSource is a generic ephemeral volume when a storage class is configured, else an emptyDir.
// Its size is pod.scratchSize, defaulting to 10Gi.
func (m moverPod) scratchSource() corev1.VolumeSource {
	class := m.spec.Pod.ScratchStorageClassName
	if class == nil {
		return corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
	}
	size := resource.MustParse(defaultScratchSize)
	if m.spec.Pod.ScratchSize != nil {
		size = *m.spec.Pod.ScratchSize
	}
	return corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{
		VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: class,
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
		}},
	}}
}

// jobSpec wraps a pod spec into a Job spec whose pods carry labels.
func jobSpec(labels map[string]string, pod corev1.PodSpec, backoffLimit int32, deadline *int64) batchv1.JobSpec {
	if deadline == nil {
		deadline = ptr.To(defaultActiveDeadline)
	}
	return batchv1.JobSpec{
		BackoffLimit:          ptr.To(backoffLimit),
		ActiveDeadlineSeconds: deadline,
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec:       pod,
		},
	}
}

func jobLabels(policy string, role jobcfg.Mode) map[string]string {
	return map[string]string{jobcfg.LabelPolicy: policy, jobcfg.LabelRole: string(role)}
}

// resolveRetention picks the policy's retention, else the store's, else the built-in default.
func resolveRetention(policy *v1.Retention, store *v1.BackupStore) snapshot.Retention {
	for _, r := range []*v1.Retention{policy, store.Spec.Retention} {
		if r == nil {
			continue
		}
		sr := snapshot.Retention{Hourly: int(r.Hourly), Daily: int(r.Daily), Weekly: int(r.Weekly), Monthly: int(r.Monthly)}
		if !sr.IsZero() {
			return sr
		}
	}
	return snapshot.Default
}

// validateSource checks that the sub-struct selected by Type is the only one set and is complete.
func validateSource(s v1.Source) error {
	set := map[v1.SourceType]bool{
		v1.SourceFiles:    s.Files != nil,
		v1.SourceSQLite:   s.SQLite != nil,
		v1.SourcePostgres: s.Postgres != nil,
		v1.SourceCommand:  s.Command != nil,
	}
	known, selected := set[s.Type]
	if !known {
		return fmt.Errorf("unknown source type %q", s.Type)
	}
	if !selected {
		return fmt.Errorf("source type %q requires spec.source.%s", s.Type, s.Type)
	}
	for t, isSet := range set {
		if isSet && t != s.Type {
			return fmt.Errorf("spec.source.%s must not be set for source type %q", t, s.Type)
		}
	}
	switch s.Type {
	case v1.SourceFiles:
		if s.Files.PVC == "" {
			return fmt.Errorf("source.files.pvc is required")
		}
	case v1.SourceSQLite:
		if s.SQLite.PVC == "" || len(s.SQLite.Paths) == 0 {
			return fmt.Errorf("source.sqlite.pvc and source.sqlite.paths are required")
		}
	case v1.SourcePostgres:
		if s.Postgres.Image == "" || s.Postgres.Host == "" {
			return fmt.Errorf("source.postgres.image and source.postgres.host are required")
		}
	case v1.SourceCommand:
		if s.Command.File == "" || len(s.Command.Command) == 0 || len(s.Command.PodSelector) == 0 {
			return fmt.Errorf("source.command.podSelector, source.command.command and source.command.file are required")
		}
	}
	return nil
}
