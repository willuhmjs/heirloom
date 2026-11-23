package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Retention is a GFS (grandfather-father-son) policy: keep the newest snapshot
// of each of the last N hours/days/ISO-weeks/months. The newest snapshot is always kept.
// At least one count must be positive; to keep only the newest snapshot use hourly: 1.
// +kubebuilder:validation:XValidation:rule="(has(self.hourly) ? self.hourly : 0) + (has(self.daily) ? self.daily : 0) + (has(self.weekly) ? self.weekly : 0) + (has(self.monthly) ? self.monthly : 0) > 0",message="set at least one of hourly, daily, weekly, monthly (hourly: 1 keeps only the newest snapshot)"
type Retention struct {
	// +kubebuilder:validation:Minimum=0
	Hourly int32 `json:"hourly,omitempty"`
	// +kubebuilder:validation:Minimum=0
	Daily int32 `json:"daily,omitempty"`
	// +kubebuilder:validation:Minimum=0
	Weekly int32 `json:"weekly,omitempty"`
	// +kubebuilder:validation:Minimum=0
	Monthly int32 `json:"monthly,omitempty"`
}

// NFSSpec points at an NFS export that receives the snapshots as plain files.
type NFSSpec struct {
	// +kubebuilder:validation:MinLength=1
	Server string `json:"server"`
	// Path of the export, e.g. /volume1/k8s-backup.
	// +kubebuilder:validation:Pattern=`^/.*`
	Path string `json:"path"`
}

// BackupStoreSpec describes where backups live.
type BackupStoreSpec struct {
	NFS NFSSpec `json:"nfs"`
	// Retention applied to policies that do not set their own.
	// Defaults to hourly=24 daily=30 weekly=12 monthly=24.
	// +optional
	Retention *Retention `json:"retention,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Server",type=string,JSONPath=`.spec.nfs.server`
// +kubebuilder:printcolumn:name="Path",type=string,JSONPath=`.spec.nfs.path`
type BackupStore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              BackupStoreSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type BackupStoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BackupStore `json:"items"`
}

// Engine selects how a policy stores its backups. snapshots (the default) writes plain hardlinked snapshot
// folders; restic stores data in a restic repository inside the policy's own store directory, so changed large
// files are stored as chunks instead of copied whole.
// +kubebuilder:validation:Enum=snapshots;restic
type Engine string

const (
	EngineSnapshots Engine = "snapshots"
	EngineRestic    Engine = "restic"
)

// SourceType selects how data is captured.
// +kubebuilder:validation:Enum=files;sqlite;postgres
type SourceType string

const (
	SourceFiles    SourceType = "files"
	SourceSQLite   SourceType = "sqlite"
	SourcePostgres SourceType = "postgres"
)

// FilesSource snapshots a directory tree of a PVC (rsync -aHAX with hardlinks to the previous snapshot).
type FilesSource struct {
	// +kubebuilder:validation:MinLength=1
	PVC string `json:"pvc"`
	// SubPath inside the PVC to back up. Defaults to the whole volume.
	// +optional
	SubPath string `json:"subPath,omitempty"`
	// Exclude patterns passed to rsync --exclude.
	// +optional
	Exclude []string `json:"exclude,omitempty"`
	// SkipACLsAndXattrs backs up and restores without ACLs and extended attributes (rsync -aH instead of
	// -aHAX). Use it when the NFS export does not support them, which makes rsync fail with exit 23.
	// +optional
	SkipACLsAndXattrs bool `json:"skipACLsAndXattrs,omitempty"`
}

// SQLiteSource takes consistent copies of SQLite databases with the online backup API.
type SQLiteSource struct {
	// +kubebuilder:validation:MinLength=1
	PVC string `json:"pvc"`
	// Paths of the database files, relative to the volume root.
	// +kubebuilder:validation:MinItems=1
	Paths []string `json:"paths"`
}

// PostgresSource dumps databases with pg_dump, run inside Image so the client always matches the server.
type PostgresSource struct {
	// Image whose pg_dump/psql/initdb match the server major version, e.g. postgres:16.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`
	// +kubebuilder:default=5432
	Port int32 `json:"port,omitempty"`
	// +kubebuilder:default=postgres
	User              string                   `json:"user,omitempty"`
	PasswordSecretRef corev1.SecretKeySelector `json:"passwordSecretRef"`
	// Databases to dump. Empty means every non-template database.
	// +optional
	Databases []string `json:"databases,omitempty"`
	// ExcludeDatabases are left out when databases is empty, for databases the backup user cannot dump
	// (for example rdsadmin on a managed server) or that are not worth backing up.
	// +optional
	ExcludeDatabases []string `json:"excludeDatabases,omitempty"`
	// Globals selects the roles and tablespaces dump (pg_dumpall --globals-only). full needs a superuser, because
	// it reads role passwords. no-role-passwords works for a least-privilege backup role; restored roles then have
	// no password and need one set. skip leaves roles out entirely.
	// +kubebuilder:validation:Enum=full;no-role-passwords;skip
	// +kubebuilder:default=full
	// +optional
	Globals string `json:"globals,omitempty"`
	// UID postgres runs as inside Image during verify (debian images 999, alpine 70).
	// +kubebuilder:default=999
	UID int64 `json:"uid,omitempty"`
}

// Source selects exactly one of files, sqlite, postgres, matching Type.
// +kubebuilder:validation:XValidation:rule="self.type != 'files' || has(self.files)",message="source.files is required when type is files"
// +kubebuilder:validation:XValidation:rule="self.type != 'sqlite' || has(self.sqlite)",message="source.sqlite is required when type is sqlite"
// +kubebuilder:validation:XValidation:rule="self.type != 'postgres' || has(self.postgres)",message="source.postgres is required when type is postgres"
// +kubebuilder:validation:XValidation:rule="(has(self.files) ? 1 : 0) + (has(self.sqlite) ? 1 : 0) + (has(self.postgres) ? 1 : 0) == 1",message="set exactly one of files, sqlite, postgres"
type Source struct {
	Type     SourceType      `json:"type"`
	Files    *FilesSource    `json:"files,omitempty"`
	SQLite   *SQLiteSource   `json:"sqlite,omitempty"`
	Postgres *PostgresSource `json:"postgres,omitempty"`
}

// Verify periodically proves snapshots are restorable.
type Verify struct {
	// Schedule (cron) of the restore test.
	Schedule string `json:"schedule"`
	// Database the SQL assertions run against. Required when sql is set and the policy does not list
	// exactly one database (the default then).
	// +optional
	Database string `json:"database,omitempty"`
	// SQL assertions for postgres: each statement must return the single value "t".
	// Example: SELECT count(*) > 100 FROM users
	// +optional
	SQL []string `json:"sql,omitempty"`
}

// Trigger runs extra backups on demand.
type Trigger struct {
	// Manual: change this string to start one backup now. status.lastManualSync is set to it when that run succeeds.
	// +optional
	Manual string `json:"manual,omitempty"`
}

// PodOptions tunes the mover pods.
type PodOptions struct {
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// +optional
	PriorityClassName string `json:"priorityClassName,omitempty"`
	// ImagePullSecrets for the mover and install images, for a private registry or mirror.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
	// Scratch space for verify (restored database) as a generic ephemeral volume; emptyDir if unset.
	// +optional
	ScratchStorageClassName *string `json:"scratchStorageClassName,omitempty"`
	// ScratchSize is the size of that ephemeral volume. Default 10Gi. Ignored for emptyDir.
	// +optional
	ScratchSize *resource.Quantity `json:"scratchSize,omitempty"`
}

// BackupPolicySpec describes one thing to back up.
// +kubebuilder:validation:XValidation:rule="self.engine != 'restic' || self.source.type == 'files'",message="engine restic only supports source.type files"
type BackupPolicySpec struct {
	// StoreRef names a cluster-scoped BackupStore.
	// +kubebuilder:validation:MinLength=1
	StoreRef string `json:"storeRef"`
	// Schedule is a cron expression.
	// +kubebuilder:validation:MinLength=1
	Schedule string `json:"schedule"`
	// TimeZone is the IANA zone the schedules are evaluated in, for example America/New_York. Default UTC.
	// Snapshot names and retention buckets are always UTC.
	// +optional
	TimeZone string `json:"timeZone,omitempty"`
	// Engine selects the backup engine. restic deduplicates changed files into chunks but makes the store
	// folder unreadable without the restic binary; snapshots writes plain folders you can browse.
	// restic requires source.type files.
	// +kubebuilder:validation:Enum=snapshots;restic
	// +kubebuilder:default=snapshots
	// +optional
	Engine Engine `json:"engine,omitempty"`
	Source Source `json:"source"`
	// Retention overrides the store default.
	// +optional
	Retention *Retention `json:"retention,omitempty"`
	// MinSourcePercent aborts a run when the source holds fewer files than this percent of the previous
	// snapshot (protects against an empty or recreated volume). 0 disables. Default 50.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=50
	MinSourcePercent int32 `json:"minSourcePercent,omitempty"`
	// +optional
	Verify *Verify `json:"verify,omitempty"`
	// +optional
	Trigger Trigger `json:"trigger,omitempty"`
	// +optional
	Suspend bool `json:"suspend,omitempty"`
	// ActiveDeadlineSeconds bounds one run. Default 3600. A first backup of a large volume may need much more:
	// a run killed at the deadline leaves nothing behind and the next one starts again from zero.
	// +kubebuilder:validation:Minimum=1
	// +optional
	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty"`
	// +optional
	Pod PodOptions `json:"pod,omitempty"`
}

// SnapshotInfo describes one snapshot directory.
type SnapshotInfo struct {
	Name  string      `json:"name"`
	Time  metav1.Time `json:"time"`
	Bytes int64       `json:"bytes,omitempty"`
	Files int64       `json:"files,omitempty"`
}

// VerifyStatus is the result of the latest verify run.
type VerifyStatus struct {
	Time     metav1.Time `json:"time"`
	OK       bool        `json:"ok"`
	Snapshot string      `json:"snapshot,omitempty"`
	Message  string      `json:"message,omitempty"`
}

type BackupPolicyStatus struct {
	LastSnapshot      *SnapshotInfo `json:"lastSnapshot,omitempty"`
	LastSyncTime      *metav1.Time  `json:"lastSyncTime,omitempty"`
	LastSyncStartTime *metav1.Time  `json:"lastSyncStartTime,omitempty"`
	// LastScheduleTime and LastVerifyScheduleTime record when the controller last started a scheduled
	// backup or verify run. The next run is due at the first schedule time after them.
	LastScheduleTime       *metav1.Time     `json:"lastScheduleTime,omitempty"`
	LastVerifyScheduleTime *metav1.Time     `json:"lastVerifyScheduleTime,omitempty"`
	LastSyncDuration       *metav1.Duration `json:"lastSyncDuration,omitempty"`
	NextSyncTime           *metav1.Time     `json:"nextSyncTime,omitempty"`
	LastManualSync         string           `json:"lastManualSync,omitempty"`
	// SnapshotsKept is the number of snapshots retained after the last run.
	SnapshotsKept int32         `json:"snapshotsKept,omitempty"`
	LastVerify    *VerifyStatus `json:"lastVerify,omitempty"`
	// Conditions: Ready (store resolved, workload rendered) and Healthy (last run succeeded).
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 40",message="name must be at most 40 characters (it is part of Job names)"
// +kubebuilder:resource:shortName=bp
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.source.type`
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Last",type=date,JSONPath=`.status.lastSyncTime`
// +kubebuilder:printcolumn:name="Snapshots",type=integer,JSONPath=`.status.snapshotsKept`
// +kubebuilder:printcolumn:name="Verified",type=string,JSONPath=`.status.lastVerify.ok`
// +kubebuilder:printcolumn:name="Healthy",type=string,JSONPath=`.status.conditions[?(@.type=="Healthy")].status`
type BackupPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              BackupPolicySpec   `json:"spec"`
	Status            BackupPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type BackupPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BackupPolicy `json:"items"`
}

// WorkloadRef names a Deployment or StatefulSet to stop during a restore.
type WorkloadRef struct {
	// +kubebuilder:validation:Enum=Deployment;StatefulSet
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// RestoreSpec restores one policy's data from a snapshot. Create it, watch status.phase.
type RestoreSpec struct {
	// PolicyRef names a BackupPolicy in the same namespace.
	// +kubebuilder:validation:MinLength=1
	PolicyRef string `json:"policyRef"`
	// From is "latest" or a snapshot name such as 2026-11-22T10-05-00Z. Default latest.
	// +kubebuilder:validation:Pattern=`^(latest|[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}-[0-9]{2}-[0-9]{2}Z)$`
	// +kubebuilder:default=latest
	From string `json:"from,omitempty"`
	// TargetPVC overrides the PVC to restore into (files and sqlite); defaults to the policy's PVC.
	// +optional
	TargetPVC string `json:"targetPVC,omitempty"`
	// Paths restricts a files restore to these sub-paths of the snapshot.
	// +optional
	Paths []string `json:"paths,omitempty"`
	// Databases restricts a postgres restore. Empty restores globals and every database.
	// +optional
	Databases []string `json:"databases,omitempty"`
	// Delete removes files in the target that are not in the snapshot (files restore).
	// +optional
	Delete bool `json:"delete,omitempty"`
	// ScaleDown workloads are scaled to zero before the restore and back afterwards.
	// For postgres, list the apps using the database, not the database itself.
	// +optional
	ScaleDown []WorkloadRef `json:"scaleDown,omitempty"`
	// ScaleUpOnFailure brings workloads back even when the restore failed. Default false: leave them down.
	// +optional
	ScaleUpOnFailure bool `json:"scaleUpOnFailure,omitempty"`
	// +optional
	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty"`
}

// +kubebuilder:validation:Enum=Pending;ScalingDown;Running;ScalingUp;Succeeded;Failed
type RestorePhase string

type ScaledWorkload struct {
	WorkloadRef `json:",inline"`
	Replicas    int32 `json:"replicas"`
}

type RestoreStatus struct {
	Phase    RestorePhase `json:"phase,omitempty"`
	Snapshot string       `json:"snapshot,omitempty"`
	Message  string       `json:"message,omitempty"`
	// Scaled records the original replica counts so they can be put back.
	Scaled    []ScaledWorkload `json:"scaled,omitempty"`
	StartTime *metav1.Time     `json:"startTime,omitempty"`
	// ScalingDownSince is when the ScalingDown phase began; it bounds how long the controller waits for workloads to stop.
	ScalingDownSince *metav1.Time `json:"scalingDownSince,omitempty"`
	CompletionTime   *metav1.Time `json:"completionTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 40",message="name must be at most 40 characters (it is part of Job names)"
// +kubebuilder:resource:shortName=brs
// +kubebuilder:printcolumn:name="Policy",type=string,JSONPath=`.spec.policyRef`
// +kubebuilder:printcolumn:name="From",type=string,JSONPath=`.status.snapshot`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.message`
type Restore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              RestoreSpec   `json:"spec"`
	Status            RestoreStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type RestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Restore `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BackupStore{}, &BackupStoreList{}, &BackupPolicy{}, &BackupPolicyList{}, &Restore{}, &RestoreList{})
}
