package vm

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestArgsKeepCallOrderAndEscapeCommas(t *testing.T) {
	var a Args
	a.Add("-name", "x").Flag("-nodefaults").Add("-drive", Option("if=none", "file="+EscapeValue("/a,b/disk.qcow2")))
	want := []string{"-name", "x", "-nodefaults", "-drive", "if=none,file=/a,,b/disk.qcow2"}
	if !reflect.DeepEqual(a.List(), want) {
		t.Fatalf("got %v", a.List())
	}
}

func TestParseAccelerators(t *testing.T) {
	out := "Accelerators supported in QEMU binary:\ntcg\nhvf\n"
	if got := ParseAccelerators(out); !reflect.DeepEqual(got, []string{"tcg", "hvf"}) {
		// the header line has a colon and capital letters, so it is not a name
		t.Fatalf("got %v", got)
	}
}

func TestSelectAcceleratorsFallsBackToTCG(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "qemu")
	os.WriteFile(fake, []byte("#!/bin/sh\nprintf 'Accelerators supported in QEMU binary:\\ntcg\\nkvm\\n'\n"), 0o755)
	if got := SelectAccelerators(context.Background(), fake, "linux", func(string) bool { return true }); !reflect.DeepEqual(got, []string{"kvm"}) {
		t.Fatalf("usable kvm: %v", got)
	}
	if got := SelectAccelerators(context.Background(), fake, "linux", func(string) bool { return false }); !reflect.DeepEqual(got, []string{"tcg"}) {
		t.Fatalf("unusable kvm: %v", got)
	}
	if got := SelectAccelerators(context.Background(), fake, "darwin", func(string) bool { return true }); !reflect.DeepEqual(got, []string{"tcg"}) {
		t.Fatalf("hvf not offered: %v", got)
	}
	if got := SelectAccelerators(context.Background(), filepath.Join(dir, "missing"), "linux", nil); got != nil {
		t.Fatalf("missing binary: %v", got)
	}
}

func fakeQMP(t *testing.T, reply func(execute string) string) Address {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "qmp.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.Write([]byte(`{"QMP":{"version":{}}}` + "\r\n"))
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					var req struct {
						Execute string `json:"execute"`
						ID      string `json:"id"`
					}
					json.Unmarshal([]byte(line), &req)
					if req.Execute == "qmp_capabilities" {
						c.Write([]byte(`{"return":{},"id":"capabilities"}` + "\r\n"))
						continue
					}
					c.Write([]byte(`{"event":"NOISE"}` + "\r\n"))
					c.Write([]byte(strings.Replace(reply(req.Execute), "$ID", req.ID, 1) + "\r\n"))
				}
			}()
		}
	}()
	return Address{Path: sock}
}

func TestQMPCommand(t *testing.T) {
	addr := fakeQMP(t, func(execute string) string {
		if execute == "boom" {
			return `{"error":{"desc":"nope"},"id":"$ID"}`
		}
		return `{"return":{"status":"running"},"id":"$ID"}`
	})
	got, err := QMPCommand(context.Background(), addr, "query-status", nil, 5*time.Second)
	if err != nil || string(got) != `{"status":"running"}` {
		t.Fatalf("got %s err %v", got, err)
	}
	if _, err := QMPCommand(context.Background(), addr, "boom", map[string]any{"a": 1}, 5*time.Second); err == nil || err.Error() != "nope" {
		t.Fatalf("err = %v", err)
	}
	Powerdown(context.Background(), addr)
}

func TestQMPTimeout(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "silent.sock")
	l, _ := net.Listen("unix", sock)
	defer l.Close()
	go func() {
		c, _ := l.Accept()
		time.Sleep(3 * time.Second)
		if c != nil {
			c.Close()
		}
	}()
	_, err := QMPCommand(context.Background(), Address{Path: sock}, "x", nil, 200*time.Millisecond)
	if err == nil || err.Error() != "QMP x timed out." {
		t.Fatalf("err = %v", err)
	}
}

func TestInstanceSocketMovesLongPathsToAPrivateDirectory(t *testing.T) {
	short, err := os.MkdirTemp("/tmp", "vmt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	addr, err := InstanceSocket(short, "vnc.sock", FreePort, "/tmp", false)
	if err != nil || addr.Path != filepath.Join(short, "vnc.sock") {
		t.Fatalf("short: %+v %v", addr, err)
	}
	long := filepath.Join(t.TempDir(), strings.Repeat("d", 120), "inst")
	os.MkdirAll(long, 0o700)
	root, err := os.MkdirTemp("/tmp", "vmr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	addr, err = InstanceSocket(long, "vnc.sock", FreePort, root, false)
	if err != nil || !strings.HasPrefix(addr.Path, root) {
		t.Fatalf("long: %+v %v", addr, err)
	}
	if st, _ := os.Stat(filepath.Dir(addr.Path)); st.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode %v", st.Mode())
	}
	// A directory that is open to others is refused rather than trusted.
	loose, _ := os.MkdirTemp("/tmp", "vml")
	t.Cleanup(func() { os.RemoveAll(loose) })
	os.MkdirAll(filepath.Join(loose, "neoagent-"+itoa(os.Getuid())), 0o755)
	os.Chmod(filepath.Join(loose, "neoagent-"+itoa(os.Getuid())), 0o755)
	if _, err := InstanceSocket(long, "vnc.sock", FreePort, loose, false); err == nil || !strings.Contains(err.Error(), "not a private directory") {
		t.Fatalf("loose directory accepted: %v", err)
	}
	win, err := InstanceSocket(short, "vnc.sock", func() (int, error) { return 6001, nil }, "/tmp", true)
	if err != nil || win.Port != 6001 {
		t.Fatalf("windows: %+v %v", win, err)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestWaitForSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	if WaitForSocket(Address{Path: sock}, 300*time.Millisecond, nil) {
		t.Fatal("reached a socket that does not exist")
	}
	if WaitForSocket(Address{Path: sock}, 5*time.Second, func() bool { return true }) {
		t.Fatal("kept waiting for a dead process")
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		l, _ := net.Listen("unix", sock)
		t.Cleanup(func() { l.Close() })
	}()
	if !WaitForSocket(Address{Path: sock}, 5*time.Second, nil) {
		t.Fatal("did not see the socket come up")
	}
}

func TestQemuImgThroughAFakeBinary(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	fake := filepath.Join(dir, "qemu-img")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\ncase \"$1\" in info) echo '{\"virtual-size\": 4294967296, \"actual-size\": 1073741824}';; esac\n"
	os.WriteFile(fake, []byte(script), 0o755)
	img := Img{Binary: fake}
	ctx := context.Background()
	if err := img.CreateOverlay(ctx, "/x/system.qcow2", "/x/base.qcow2", "qcow2"); err != nil {
		t.Fatal(err)
	}
	if err := img.Create(ctx, "/x/data.qcow2", "12G"); err != nil {
		t.Fatal(err)
	}
	if err := img.EnsureSize(ctx, "/x/system.qcow2", 8); err != nil {
		t.Fatal(err)
	}
	if err := img.EnsureSize(ctx, "/x/system.qcow2", 2); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(log)
	want := "create -f qcow2 -F qcow2 -b /x/base.qcow2 /x/system.qcow2\n" +
		"create -f qcow2 /x/data.qcow2 12G\n" +
		"info --output=json -U /x/system.qcow2\nresize /x/system.qcow2 8G\n" +
		"info --output=json -U /x/system.qcow2\n"
	if string(calls) != want {
		t.Fatalf("calls:\n%s\nwant:\n%s", calls, want)
	}
}

func TestSparseLiabilitySkipsFrozenDisks(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "qemu-img")
	os.WriteFile(fake, []byte("#!/bin/sh\necho '{\"virtual-size\": 3000, \"actual-size\": 1000}'\n"), 0o755)
	inst := filepath.Join(dir, "inst")
	os.MkdirAll(inst, 0o700)
	for _, n := range []string{"system.qcow2", "data.qcow2", "system.previous.qcow2", "notes.txt"} {
		os.WriteFile(filepath.Join(inst, n), nil, 0o600)
	}
	got := Img{Binary: fake}.SparseLiability(context.Background(), []string{inst, inst}, func(p string) bool {
		return filepath.Base(p) == "system.previous.qcow2"
	})
	if got != 4000 {
		t.Fatalf("liability = %d", got)
	}
}

func TestParseOrphansOnlyMatchesThisInstance(t *testing.T) {
	ps := "  101 /usr/bin/qemu-system-x86_64 -name a /data/inst1/system.qcow2\n" +
		"  102 /usr/bin/qemu-system-aarch64 -drive /data/inst2/data.qcow2\n" +
		"  103 vim /data/inst1/notes\n" +
		"    1 /usr/bin/qemu-system-x86_64 /data/inst1/x\n" +
		"  777 /usr/bin/qemu-system-x86_64 /data/inst1/me\n"
	if got := parseOrphans(ps, "/data/inst1", 777); !reflect.DeepEqual(got, []int{101}) {
		t.Fatalf("got %v", got)
	}
}

func TestHypervisorEntitlementsPlist(t *testing.T) {
	if !strings.Contains(HypervisorEntitlements, "<key>com.apple.security.hypervisor</key>\n  <true/>") {
		t.Fatal("plist lost the hypervisor key")
	}
}

func TestResolveExecutable(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "tool")
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	plain := filepath.Join(dir, "plain")
	os.WriteFile(plain, []byte("x"), 0o644)
	if got := ResolveExecutable("sh-not-there-xyz", []string{plain, "", bin}); got != bin {
		t.Fatalf("got %q", got)
	}
	if got := ResolveExecutable("sh", []string{plain}); got != "sh" {
		t.Fatalf("got %q", got)
	}
	if ResolveExecutable("definitely-not-a-command", nil) != "" {
		t.Fatal("resolved a missing command")
	}
	if !IsRunnableCommand(bin) || IsRunnableCommand(plain) || IsRunnableCommand("") {
		t.Fatal("runnable checks are wrong")
	}
}

func TestDiskCapacity(t *testing.T) {
	avail, total := DiskCapacity(t.TempDir())
	if avail <= 0 || total < avail {
		t.Fatalf("avail %d total %d", avail, total)
	}
	if a, tt := DiskCapacity("/definitely/not/here"); a != 0 || tt != 0 {
		t.Fatal("missing directory should report zeros")
	}
}
