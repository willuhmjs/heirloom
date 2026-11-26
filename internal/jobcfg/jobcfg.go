// Package jobcfg is the contract between the controller (which renders mover pods) and the runner
// (which executes inside them).
//
// Pod shape produced by the controller, for every mover Job (backup, verify, restore):
//
//	initContainer "install"  image=$HEIRLOOM_IMAGE  copies /heirloom to the shared emptyDir /tools
//	container     "mover"    image=<runner image, or postgres.image for postgres sources>
//	                         command: /tools/heirloom run
//	                         env BS_CONFIG=<JSON of Config>, plus PGPASSWORD from the secretKeyRef (postgres)
//	                         terminationMessagePath=/dev/termination-log, policy FallbackToLogsOnError
//	volumes (mount paths inside "mover"):
//	  /store    ONLY this policy's directory of the NFS export (volumeMount subPath <namespace>/<policy>), so a
//	            mover can never see another namespace's backups (writable: restic writes repository locks)
//	  /source   the PVC, files/sqlite only (read-only for files backup, read-write for sqlite and restore)
//	  /scratch  emptyDir or generic ephemeral volume, verify only
//
// The runner runs as root (needed to preserve ownership and to chown scratch for postgres).
package jobcfg

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	v1 "github.com/willuhmjs/heirloom/api/v1"
	"github.com/willuhmjs/heirloom/internal/snapshot"
)

const (
	EnvConfig = "BS_CONFIG"
	// EnvRepoPassword is restic's own variable for the repository password.
	EnvRepoPassword = "RESTIC_PASSWORD"
	// KeySecretKey is the key of the per-policy Secret that holds the derived repository password.
	KeySecretKey = "password"

	StoreMount   = "/store"
	SourceMount  = "/source"
	ScratchMount = "/scratch"
	ToolsMount   = "/tools"
	ToolsBinary  = "/tools/heirloom"
	// ImageBinary is where the runner image keeps the static binary.
	ImageBinary = "/heirloom"

	LabelPolicy = "heirloom.io/policy"
	LabelRole   = "heirloom.io/role" // backup | verify | restore

	ContainerMover   = "mover"
	ContainerInstall = "install"
)

type Mode string

const (
	ModeBackup  Mode = "backup"
	ModeVerify  Mode = "verify"
	ModeRestore Mode = "restore"
)

// Restore carries restore-only options.
type Restore struct {
	// From is "latest" or a restic snapshot id (hex).
	From      string   `json:"from"`
	Paths     []string `json:"paths,omitempty"`
	Databases []string `json:"databases,omitempty"`
	Delete    bool     `json:"delete,omitempty"`
}

// Config is JSON-encoded into BS_CONFIG.
type Config struct {
	Mode      Mode   `json:"mode"`
	Namespace string `json:"namespace"`
	Policy    string `json:"policy"`
	// Spec is the full policy spec (source, minSourcePercent, verify, ...).
	Spec v1.BackupPolicySpec `json:"spec"`
	// Retention is already resolved (policy, else store, else default).
	Retention snapshot.Retention `json:"retention"`
	Restore   *Restore           `json:"restore,omitempty"`
	// Encrypted means the repository has a password, passed in EnvRepoPassword. The mover then refuses to
	// run without it rather than fall back to a passwordless repository.
	Encrypted bool `json:"encrypted,omitempty"`
}

// StoreSubPath is the location of a policy's snapshots inside the NFS export, e.g. "<ns>/<policy>".
func StoreSubPath(namespace, policy string) string { return namespace + "/" + policy }

// PolicyDir is where the mover sees this policy's snapshots. The controller mounts only StoreSubPath of the
// export at StoreMount, so the mount root is the policy directory.
func (c Config) PolicyDir() string { return StoreMount }

func (c Config) Encode() (string, error) {
	b, err := json.Marshal(c)
	return string(b), err
}

func FromEnv() (Config, error) {
	var c Config
	raw := os.Getenv(EnvConfig)
	if raw == "" {
		return c, fmt.Errorf("%s is not set", EnvConfig)
	}
	return c, json.Unmarshal([]byte(raw), &c)
}

// Result is written as JSON to /dev/termination-log. The controller reads it from the pod status and
// copies it into the policy/restore status; the controller never mounts the store.
type Result struct {
	Mode     Mode      `json:"mode"`
	OK       bool      `json:"ok"`
	Message  string    `json:"message,omitempty"`
	Snapshot string    `json:"snapshot,omitempty"`
	Time     time.Time `json:"time"`
	Duration float64   `json:"durationSeconds"`
	Bytes    int64     `json:"bytes,omitempty"`
	Files    int64     `json:"files,omitempty"`
	// Added is what the run added to the repository after dedup; RepoBytes the repository's size afterwards.
	Added     int64 `json:"added,omitempty"`
	RepoBytes int64 `json:"repoBytes,omitempty"`
	// Kept and Pruned are snapshot counts after retention (backup mode).
	Kept   int `json:"kept,omitempty"`
	Pruned int `json:"pruned,omitempty"`
}

const TerminationLog = "/dev/termination-log"
