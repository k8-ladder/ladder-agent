package gitea

import "testing"

func TestAllowedPath(t *testing.T) {
	const prefix = "/api/v1/repos/ladder/deploy"
	tests := []struct {
		path string
		want bool
	}{
		{prefix, true},
		{prefix + "/contents/apps/shop-api/values.yaml", true},
		{prefix + "/contents/apps/my%20app/values.yaml", true},
		{prefix + "/pulls", true},
		{prefix + "2/contents", false},
		{"/api/v1/repos/ladder/other", false},
		{"/api/v1/admin/users", false},
		{prefix + "/../other", false},
		{prefix + "/%2e%2e/other", false},
		{prefix + "/contents/./x", false},
		{prefix + "//contents", false},
		{prefix + "/contents/", false},
		{prefix + "/contents?ref=main", false},
		{prefix + "/contents#x", false},
		{prefix + "/%zz", false},
	}
	for _, tt := range tests {
		if got := allowedPath(tt.path, prefix); got != tt.want {
			t.Errorf("allowedPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}
