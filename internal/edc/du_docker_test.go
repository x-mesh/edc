package edc

import (
	"strings"
	"testing"
)

func TestDuContainerLabelsReadInspectAndMountinfo(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	inspect := strings.Join([]string{
		a + `|/api-1|/var/lib/docker/containers/` + a + `/` + a + `-json.log|/var/lib/docker/overlay2/l1/diff|[{"Type":"volume","Name":"data","Source":"/var/lib/docker/volumes/data/_data"},{"Type":"bind","Source":"/srv/shared"}]`,
		b + `|/db|/var/lib/docker/containers/` + b + `/` + b + `-json.log||[{"Type":"volume","Name":"data","Source":"/var/lib/docker/volumes/data/_data"},{"Type":"bind","Source":"/srv/shared"},{"Type":"bind","Source":"/srv/db/"}]`,
		`cccc|/api|/var/lib/docker/containers/cccc/cccc-json.log||[{"Type":"bind","Source":"/srv/shared"}]`,
		"Error: No such container: dddd",
	}, "\n")
	mountinfo := strings.Join([]string{
		"100 1 0:50 / /var/lib/docker/rootfs/overlayfs/" + b + " rw,relatime - overlay overlay rw,lowerdir=/x:/y,upperdir=/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/119/fs,workdir=/w",
		"101 1 0:51 / /var/lib/docker/rootfs/overlayfs/" + strings.Repeat("e", 64) + " rw - overlay overlay rw,upperdir=/u/fs",
		"102 1 8:3 / / rw - xfs /dev/sda3 rw",
	}, "\n")
	labels := duContainerLabels(inspect, mountinfo)
	want := map[string]string{
		"/var/lib/docker/containers/" + a:                                          "container api-1",
		"/var/lib/docker/overlay2/l1":                                              "writable layer of api-1",
		"/var/lib/docker/overlay2/l1/merged":                                       "root file system of api-1",
		"/var/lib/docker/containers/" + b:                                          "container db",
		"/var/lib/docker/rootfs/overlayfs/" + b:                                    "root file system of db",
		"/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/119": "writable layer of db",
		"/var/lib/docker/containers/cccc":                                          "container api",
		"/var/lib/docker/volumes/data":                                             "volume data of api-1, db",
		"/srv/shared":                                                              "mounted in api, api-1, db",
		"/srv/db":                                                                  "mounted in db",
	}
	if len(labels) != len(want) {
		t.Errorf("%d labels, want %d: %v", len(labels), len(want), labels)
	}
	for path, label := range want {
		if labels[path] != label {
			t.Errorf("%s = %q, want %q", path, labels[path], label)
		}
	}
	if got := duContainerLabels("", ""); len(got) != 0 {
		t.Fatalf("labels without docker = %v", got)
	}
}
