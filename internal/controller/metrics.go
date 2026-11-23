package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/willuhmjs/backshots/api/v1alpha1"
)

var (
	labels = []string{"namespace", "policy"}

	lastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "backshots_last_success_timestamp_seconds",
		Help: "Unix time of the last successful backup.",
	}, labels)
	lastSnapshotBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "backshots_last_snapshot_bytes",
		Help: "Size in bytes of the last snapshot.",
	}, labels)
	lastVerifyOK = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "backshots_last_verify_ok",
		Help: "1 if the last verify run succeeded, 0 if it failed.",
	}, labels)
	snapshotsKept = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "backshots_snapshots_kept",
		Help: "Snapshots retained after the last backup.",
	}, labels)

	policyHealthy = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "backshots_policy_healthy",
		Help: "1 if the policy's Healthy condition is True, 0 if False (failed or stale). Absent while Unknown. " +
			"Unlike last_success_timestamp it exists for a policy that never succeeded.",
	}, labels)
	policyCreated = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "backshots_policy_created_timestamp_seconds",
		Help: "Unix time the policy was created.",
	}, labels)

	allGauges = []*prometheus.GaugeVec{lastSuccess, lastSnapshotBytes, lastVerifyOK, snapshotsKept, policyHealthy, policyCreated}
)

func init() {
	for _, g := range allGauges {
		metrics.Registry.MustRegister(g)
	}
}

// recordMetrics mirrors the policy status into gauges; series without data are removed.
func recordMetrics(p *v1alpha1.BackupPolicy) {
	st := p.Status
	set := func(g *prometheus.GaugeVec, v float64, ok bool) {
		if ok {
			g.WithLabelValues(p.Namespace, p.Name).Set(v)
		} else {
			g.DeleteLabelValues(p.Namespace, p.Name)
		}
	}
	if st.LastSyncTime != nil {
		set(lastSuccess, float64(st.LastSyncTime.Unix()), true)
	} else {
		set(lastSuccess, 0, false)
	}
	if st.LastSnapshot != nil {
		set(lastSnapshotBytes, float64(st.LastSnapshot.Bytes), true)
	} else {
		set(lastSnapshotBytes, 0, false)
	}
	if st.LastVerify != nil {
		v := 0.0
		if st.LastVerify.OK {
			v = 1
		}
		set(lastVerifyOK, v, true)
	} else {
		set(lastVerifyOK, 0, false)
	}
	set(snapshotsKept, float64(st.SnapshotsKept), st.LastSnapshot != nil)
	set(policyCreated, float64(p.CreationTimestamp.Unix()), true)
	switch c := meta.FindStatusCondition(st.Conditions, condHealthy); {
	case c != nil && c.Status == metav1.ConditionTrue:
		set(policyHealthy, 1, true)
	case c != nil && c.Status == metav1.ConditionFalse:
		set(policyHealthy, 0, true)
	default:
		set(policyHealthy, 0, false)
	}
}

func forgetMetrics(namespace, policy string) {
	for _, g := range allGauges {
		g.DeleteLabelValues(namespace, policy)
	}
}
