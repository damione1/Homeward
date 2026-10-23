package server

import (
	"bytes"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// The delivered Linux image need not contain ps. Both platforms measure the
// actual process RSS, with a birth-identity check around the RSS acquisition.
func nha165ProcessSample(pid int) (string, uint64, error) {
	if pid <= 0 {
		return "", 0, errors.New("invalid core PID")
	}
	if runtime.GOOS == "linux" {
		return nha165LinuxProcessSample("/proc", pid)
	}
	birth := func() (string, error) {
		raw, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return "", err
		}
		value := strings.TrimSpace(string(raw))
		if value == "" {
			return "", errors.New("missing core birth identity")
		}
		return value, nil
	}
	before, err := birth()
	if err != nil {
		return "", 0, err
	}
	raw, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", 0, err
	}
	kib, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || kib == 0 || kib > math.MaxUint64/1024 {
		return "", 0, errors.New("invalid core RSS")
	}
	after, err := birth()
	if err != nil {
		return "", 0, err
	}
	if before != after {
		return "", 0, errors.New("core PID birth identity changed during RSS sample")
	}
	return before, kib * 1024, nil
}

func nha165LinuxBirth(raw []byte, pid int) (string, error) {
	// comm (field 2) can itself contain spaces and parentheses. Its final ')'
	// precedes state (field 3); starttime is field 22, index 19 after comm.
	open, close := bytes.IndexByte(raw, '('), bytes.LastIndexByte(raw, ')')
	if open < 1 || close <= open || strings.TrimSpace(string(raw[:open])) != strconv.Itoa(pid) {
		return "", errors.New("invalid proc PID/comm")
	}
	fields := strings.Fields(string(raw[close+1:]))
	if len(fields) < 20 {
		return "", errors.New("missing proc starttime")
	}
	ticks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || ticks == 0 {
		return "", errors.New("invalid proc starttime")
	}
	return strconv.FormatUint(ticks, 10), nil
}

func nha165LinuxRSS(raw []byte) (uint64, error) {
	var rss uint64
	found := false
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "VmRSS:" {
			continue
		}
		if found || len(fields) != 3 || fields[2] != "kB" {
			return 0, errors.New("invalid proc RSS unit or duplicate")
		}
		kib, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil || kib == 0 || kib > math.MaxUint64/1024 {
			return 0, errors.New("invalid proc RSS value")
		}
		rss, found = kib*1024, true
	}
	if !found {
		return 0, errors.New("missing proc RSS")
	}
	return rss, nil
}

func nha165LinuxProcessSample(procRoot string, pid int) (string, uint64, error) {
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	birth := func() (string, error) {
		raw, err := os.ReadFile(filepath.Join(dir, "stat"))
		if err != nil {
			return "", err
		}
		return nha165LinuxBirth(raw, pid)
	}
	before, err := birth()
	if err != nil {
		return "", 0, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "status"))
	if err != nil {
		return "", 0, err
	}
	rss, err := nha165LinuxRSS(raw)
	if err != nil {
		return "", 0, err
	}
	after, err := birth()
	if err != nil {
		return "", 0, err
	}
	if before != after {
		return "", 0, errors.New("core PID birth identity changed during RSS sample")
	}
	return before, rss, nil
}

func TestNHA165ProcessMeasurementRejectsInvalidProcIdentityAndUnits(t *testing.T) {
	stat := "42 (core (test) name)) S " + strings.Repeat("0 ", 18) + "12345 0 0\n"
	birth, err := nha165LinuxBirth([]byte(stat), 42)
	if err != nil || birth != "12345" {
		t.Fatalf("valid comm with parentheses: birth=%q err=%v", birth, err)
	}
	for _, raw := range []string{stat, "42 (core) S 0", strings.Replace(stat, "12345", "-1", 1), strings.Replace(stat, "12345", "0", 1)} {
		pid := 42
		if raw == stat {
			pid = 43
		}
		if _, err := nha165LinuxBirth([]byte(raw), pid); err == nil {
			t.Fatalf("accepted invalid birth record: %q PID=%d", raw, pid)
		}
	}
	for _, raw := range []string{"", "VmRSS: 1 MB\n", "VmRSS: -1 kB\n", "VmRSS: 0 kB\n", "VmRSS: 18446744073709551615 kB\n", "VmRSS: 1 kB\nVmRSS: 2 kB\n"} {
		if _, err := nha165LinuxRSS([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid RSS record: %q", raw)
		}
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "42"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"stat": stat, "status": "Name:\tcore\nVmRSS:\t262144 kB\n"} {
		if err := os.WriteFile(filepath.Join(dir, "42", name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	gotBirth, rss, err := nha165LinuxProcessSample(dir, 42)
	if err != nil || gotBirth != birth || rss != nha165RSSLimit {
		t.Fatalf("sample at exact RSS ceiling: %s %d %v", gotBirth, rss, err)
	}
	if err := os.Remove(filepath.Join(dir, "42", "stat")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := nha165LinuxProcessSample(dir, 42); err == nil {
		t.Fatal("accepted disappeared process")
	}
	if runtime.GOOS == "linux" {
		if birth, rss, err := nha165ProcessSample(os.Getpid()); err != nil || birth == "" || rss == 0 {
			t.Fatalf("live Linux self RSS: birth=%q RSS=%d error=%v", birth, rss, err)
		}
	}
}
