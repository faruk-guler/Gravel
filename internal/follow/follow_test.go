package follow

import "testing"

func TestResolve(t *testing.T) {
	const (
		cwd  = "/home/user/projects"
		home = "/home/user"
	)
	cases := []struct {
		name string
		line string
		want string
		ok   bool
	}{
		{"absolute", "cd /var/log", "/var/log", true},
		{"absolute trailing slash", "cd /var/log/", "/var/log", true},
		{"parent", "cd ..", "/home/user", true},
		{"two levels up", "cd ../..", "/home", true},
		{"no argument goes home", "cd", "/home/user", true},
		{"tilde", "cd ~", "/home/user", true},
		{"tilde subpath", "cd ~/logs", "/home/user/logs", true},
		{"relative", "cd api", "/home/user/projects/api", true},
		{"relative nested", "cd a/b", "/home/user/projects/a/b", true},
		{"dot", "cd .", "/home/user/projects", true},
		{"quoted space", `cd "my dir"`, "/home/user/projects/my dir", true},
		{"single quoted", `cd 'my dir'`, "/home/user/projects/my dir", true},
		{"dotdot inside", "cd ./x/../y", "/home/user/projects/y", true},
		{"root", "cd /", "/", true},
		{"chained after echo", "echo hi && cd /tmp", "/tmp", true},
		{"chained before ls", "cd /etc && ls", "/etc", true},
		{"last cd wins", "cd /a; cd /b", "/b", true},
		{"semicolon form", "cd /var; echo done", "/var", true},
		{"backgrounded", "cd /tmp &", "/tmp", true},
		// Anything we cannot name with confidence must leave the browser alone.
		{"no cd", "git status", "", false},
		{"pipeline", "cd /var | cat", "", false},
		{"parameter expansion", "cd $HOME/x", "", false},
		{"redirection", "cd /var 2>/dev/null", "", false},
		{"tab completion", "cd /var/l\t", "", false},
		{"previous directory", "cd -", "", false},
		{"user expansion", "cd ~root", "", false},
		{"extra argument", "cd a b", "", false},
		{"unterminated quote", `cd "oops`, "", false},
		{"sudo cd is not a cd", "sudo cd /tmp", "", false},
		{"empty line", "", "", false},
		{"unrelated separator only", "ls -a", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Resolve(cwd, home, tc.line)
			if ok != tc.ok {
				t.Fatalf("Resolve(%q) ok = %v, want %v (got %q)", tc.line, ok, tc.ok, got)
			}
			if got != tc.want {
				t.Fatalf("Resolve(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

func TestResolveHandlesEmptyContext(t *testing.T) {
	// A session that has not browsed anywhere yet still resolves relative to /.
	if got, ok := Resolve("", "", "cd logs"); !ok || got != "/logs" {
		t.Fatalf("empty cwd/home: got %q ok=%v, want %q", got, ok, "/logs")
	}
	// Missing home degrades to root rather than producing an empty path.
	if got, ok := Resolve("/srv", "", "cd"); !ok || got != "/" {
		t.Fatalf("bare cd with no home: got %q ok=%v, want %q", got, ok, "/")
	}
}
