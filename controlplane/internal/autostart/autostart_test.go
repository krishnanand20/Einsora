package autostart

import (
	"encoding/xml"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func options(goos string) Options {
	return Options{
		GOOS: goos, Home: "/Users/ada", UID: 501, Binary: "/opt/homebrew/bin/einsora",
		DataDir: "/Users/ada/Library/Application Support/Einsora",
		Env:     []Var{{"PATH", "/opt/homebrew/bin:/usr/bin:/bin"}, {"HF_TOKEN", `a<b>&"c"`}},
	}
}

// plist is a launchd property list's top-level dict, as key → raw value element.
func plist(t *testing.T, data []byte) map[string]string {
	t.Helper()
	var doc struct {
		Dict struct {
			Items []struct {
				XMLName xml.Name
				Inner   string `xml:",innerxml"`
				Chars   string `xml:",chardata"`
			} `xml:",any"`
		} `xml:"dict"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("not XML: %v\n%s", err, data)
	}
	out := map[string]string{}
	items := doc.Dict.Items
	for i := 0; i+1 < len(items); i += 2 {
		if items[i].XMLName.Local != "key" {
			t.Fatalf("expected a key, got %s", items[i].XMLName.Local)
		}
		v := items[i+1]
		out[items[i].Chars] = v.XMLName.Local + ":" + strings.TrimSpace(v.Inner)
	}
	return out
}

func TestLaunchAgent(t *testing.T) {
	d, err := Define(options("darwin"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Path != "/Users/ada/Library/LaunchAgents/com.einsora.up.plist" {
		t.Errorf("path = %s", d.Path)
	}
	keys := plist(t, d.Content)
	if keys["Label"] != "string:com.einsora.up" || keys["RunAtLoad"] != "true:" {
		t.Errorf("label or RunAtLoad: %v", keys)
	}
	for _, want := range []string{
		"<string>/opt/homebrew/bin/einsora</string>", "<string>up</string>", "<string>-no-open</string>",
		"<key>SuccessfulExit</key>", // restarted after a crash, not after a clean stop
		"<string>a&lt;b&gt;&amp;&#34;c&#34;</string>",
		"<string>/Users/ada/Library/Application Support/Einsora/service.log</string>",
	} {
		if !strings.Contains(string(d.Content), want) {
			t.Errorf("plist lacks %s:\n%s", want, d.Content)
		}
	}
	if !slices.Equal(d.Load[1], []string{"launchctl", "bootstrap", "gui/501", d.Path}) ||
		!slices.Equal(d.Unload[0], []string{"launchctl", "bootout", "gui/501/com.einsora.up"}) {
		t.Errorf("commands: %v %v", d.Load, d.Unload)
	}
	if runtime.GOOS == "darwin" {
		// macOS's own check of the file launchd will read.
		file := filepath.Join(t.TempDir(), "agent.plist")
		if err := os.WriteFile(file, d.Content, 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("/usr/bin/plutil", "-lint", file).CombinedOutput(); err != nil {
			t.Errorf("plutil: %v: %s", err, out)
		}
	}
}

func TestSystemdUserService(t *testing.T) {
	o := options("linux")
	o.Home, o.Binary, o.DataDir = "/home/ada", "/usr/local/bin/ein 100%$x", "/home/ada/.local/share/einsora"
	o.Env = []Var{{"PATH", "/usr/local/bin:/usr/bin"}, {"HF_TOKEN", `pa$$"wo%d\`}}
	d, err := Define(o)
	if err != nil {
		t.Fatal(err)
	}
	if d.Path != "/home/ada/.config/systemd/user/einsora.service" {
		t.Errorf("path = %s", d.Path)
	}
	for _, want := range []string{
		`ExecStart="/usr/local/bin/ein 100%%$$x" up -no-open`,
		`Environment="PATH=/usr/local/bin:/usr/bin"`,
		`Environment="HF_TOKEN=pa$$\"wo%%d\\"`, // $ isn't expanded in Environment=, % is
		"Restart=on-failure", "WantedBy=default.target",
	} {
		if !strings.Contains(string(d.Content), want) {
			t.Errorf("unit lacks %s:\n%s", want, d.Content)
		}
	}
	o.ConfigHome = "/home/ada/cfg"
	if d, _ := Define(o); d.Path != "/home/ada/cfg/systemd/user/einsora.service" {
		t.Errorf("XDG_CONFIG_HOME ignored: %s", d.Path)
	}
	o.ConfigHome = "relative"
	if d, _ := Define(o); d.Path != "/home/ada/.config/systemd/user/einsora.service" {
		t.Errorf("a relative XDG_CONFIG_HOME was used: %s", d.Path)
	}
}

func TestDefineRefusesWhatCantBeWrittenSafely(t *testing.T) {
	for name, change := range map[string]func(*Options){
		"relative binary":    func(o *Options) { o.Binary = "einsora" },
		"relative data":      func(o *Options) { o.DataDir = "data" },
		"newline in a path":  func(o *Options) { o.Binary = "/bin/ein\nsora" },
		"newline in a value": func(o *Options) { o.Env = []Var{{"X", "a\nb"}} },
		"bad name":           func(o *Options) { o.Env = []Var{{"A-B", "x"}} },
		"empty name":         func(o *Options) { o.Env = []Var{{"", "x"}} },
		"twice":              func(o *Options) { o.Env = []Var{{"X", "1"}, {"X", "2"}} },
		"windows":            func(o *Options) { o.GOOS = "windows" },
	} {
		o := options("linux")
		change(&o)
		if _, err := Define(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSearchPath(t *testing.T) {
	got := SearchPath("/opt/homebrew/bin:bin:/usr/bin/:/usr/bin::/opt/homebrew/bin:/bad\npath")
	if got != "/opt/homebrew/bin:/usr/bin" {
		t.Errorf("SearchPath = %q", got)
	}
}
