package models

import "testing"

func TestNormalizeClusterName(t *testing.T) {
	ds := &Datasource{ClusterName: "no_assigned_engine", SettingsJson: map[string]interface{}{
		"pgsql.cluster_name": "no_assigned_engine",
	}}
	ds.NormalizeClusterName()
	if ds.ClusterName != "default" {
		t.Fatalf("cluster_name=%q, want default", ds.ClusterName)
	}
	if ds.SettingsJson["pgsql.cluster_name"] != "default" {
		t.Fatalf("settings cluster=%v", ds.SettingsJson["pgsql.cluster_name"])
	}

	keep := &Datasource{ClusterName: "edge-a"}
	keep.NormalizeClusterName()
	if keep.ClusterName != "edge-a" {
		t.Fatalf("kept cluster mutated: %q", keep.ClusterName)
	}
}