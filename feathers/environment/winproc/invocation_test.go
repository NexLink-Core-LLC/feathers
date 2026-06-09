//go:build windows

package winproc

import (
	"reflect"
	"testing"
)

func TestSubstitutePlaceholders(t *testing.T) {
	env := map[string]string{
		"SERVER_MEMORY": "1024",
		"SERVER_PORT":   "25565",
		"SERVER_IP":     "0.0.0.0",
	}
	cases := []struct {
		in, want string
	}{
		{"java -Xmx{{SERVER_MEMORY}}M -jar server.jar", "java -Xmx1024M -jar server.jar"},
		{"app --port {{ SERVER_PORT }}", "app --port 25565"},
		{"bind {{SERVER_IP}}:{{SERVER_PORT}}", "bind 0.0.0.0:25565"},
		{"echo {{UNKNOWN}} done", "echo  done"}, // unknown -> empty
		{"no placeholders here", "no placeholders here"},
	}
	for _, c := range cases {
		if got := substitutePlaceholders(c.in, env); got != c.want {
			t.Errorf("substitutePlaceholders(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTokenize(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`java -Xmx1024M -jar server.jar nogui`, []string{"java", "-Xmx1024M", "-jar", "server.jar", "nogui"}},
		{`"C:\Program Files\Java\java.exe" -jar app.jar`, []string{`C:\Program Files\Java\java.exe`, "-jar", "app.jar"}},
		{`app --name 'My Server' --port 25565`, []string{"app", "--name", "My Server", "--port", "25565"}},
		{`   spaced    out   args  `, []string{"spaced", "out", "args"}},
		{``, nil},
	}
	for _, c := range cases {
		if got := tokenize(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("tokenize(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestBuildCommand(t *testing.T) {
	vars := []string{
		"STARTUP=java -Xmx{{SERVER_MEMORY}}M -jar server.jar nogui",
		"SERVER_MEMORY=2048",
		"SERVER_PORT=25565",
	}
	argv, err := buildCommand(vars)
	if err != nil {
		t.Fatalf("buildCommand returned error: %v", err)
	}
	want := []string{"java", "-Xmx2048M", "-jar", "server.jar", "nogui"}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("buildCommand = %#v, want %#v", argv, want)
	}

	if _, err := buildCommand([]string{"SERVER_MEMORY=2048"}); err == nil {
		t.Error("buildCommand with no STARTUP should return an error")
	}
	if _, err := buildCommand([]string{"STARTUP=   "}); err == nil {
		t.Error("buildCommand with blank STARTUP should return an error")
	}
}
