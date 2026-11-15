package sessionformat

import "testing"

func TestProjectKeyMatchesDeepSeekJSONLFormat(t *testing.T) {
	for _, test := range []struct{ cwd, want string }{
		{"/", "--root--"},
		{"/root/harness/workspaces/first", "--root-harness-workspaces-first--"},
		{"/root/harness/a b", "--root-harness-a~0020b--"},
		{"/tmp/😀", "--tmp-~D83D~DE00--"},
		{"/tmp/a~b", "--tmp-a~007Eb--"},
	} {
		got, err := ProjectKey(test.cwd)
		if err != nil || got != test.want {
			t.Errorf("ProjectKey(%q) = %q, %v; want %q", test.cwd, got, err, test.want)
		}
	}
}
