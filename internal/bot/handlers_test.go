package bot

import "testing"

func TestParseCallback(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		wantUID int64
		wantIdx int
		wantOK  bool
	}{
		{"valid", "cap:12345:3", 12345, 3, true},
		{"negative user id", "cap:-1001234:0", -1001234, 0, true},
		{"wrong prefix", "foo:1:2", 0, 0, false},
		{"not enough parts", "cap:1", 0, 0, false},
		{"too many parts", "cap:1:2:3", 0, 0, false},
		{"bad user id", "cap:abc:1", 0, 0, false},
		{"bad index", "cap:1:x", 0, 0, false},
		{"empty", "", 0, 0, false},
		{"trailing garbage", "cap:1:2trailing", 0, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			uid, idx, ok := parseCallback(tc.data)
			if ok != tc.wantOK || uid != tc.wantUID || idx != tc.wantIdx {
				t.Fatalf("parseCallback(%q) = (%d, %d, %v), want (%d, %d, %v)",
					tc.data, uid, idx, ok, tc.wantUID, tc.wantIdx, tc.wantOK)
			}
		})
	}
}
