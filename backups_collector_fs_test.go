package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type backupFilesystemRunner struct {
	root           string
	reads          int
	streams        int
	streamCommands [][]string
	afterRead      func(int)
	transform      func([]byte) []byte
}

func (r *backupFilesystemRunner) command(ctx context.Context, args []string) *exec.Cmd {
	for i, arg := range args {
		if arg == "--" {
			local := append([]string(nil), args[i+1:]...)
			for j := range local {
				local[j] = strings.ReplaceAll(local[j], backupDataRoot, r.root)
				local[j] = strings.ReplaceAll(local[j], "'/home/steam'", shellQuote(filepath.Dir(r.root)))
				local[j] = strings.ReplaceAll(local[j], "'/home'", shellQuote(filepath.Dir(filepath.Dir(r.root))))
			}
			return exec.CommandContext(ctx, local[0], local[1:]...)
		}
	}
	panic("expected pod exec")
}

func (r *backupFilesystemRunner) Run(ctx context.Context, _ string, args ...string) ([]byte, error) {
	cmd := r.command(ctx, args)
	output, err := cmd.Output()
	if strings.Contains(strings.Join(args, " "), "sha256sum") {
		r.reads++
		if r.afterRead != nil {
			r.afterRead(r.reads)
		}
	}
	return bytes.ReplaceAll(output, []byte(r.root), []byte(backupDataRoot)), err
}

func (r *backupFilesystemRunner) RunStream(ctx context.Context, destination io.Writer, _ string, args ...string) error {
	r.streams++
	r.streamCommands = append(r.streamCommands, append([]string(nil), args...))
	output, err := r.command(ctx, args).Output()
	if err != nil {
		return err
	}
	if r.transform != nil {
		output = r.transform(output)
	}
	_, err = destination.Write(output)
	return err
}

func TestRunningDirectoryArchiveUsesPortableSaveExclusion(t *testing.T) {
	k, r := backupFilesystem(t)
	writeBackupFixture(t, r.root, "saves/World.sav.backup", "world")
	_, _, err := k.captureBackupItem(context.Background(), backupKubeTarget(), BackupItemSpec{Name: "world", Kind: BackupItemDirectory}, BackupSourceRule{Path: "saves"}, BackupServerRunning, t.TempDir(), 102400)
	if err != nil {
		t.Fatalf("capture directory: %v", err)
	}
	if len(r.streamCommands) != 1 {
		t.Fatalf("stream commands = %d, want 1", len(r.streamCommands))
	}
	script := strings.Join(r.streamCommands[0], " ")
	if strings.Contains(script, "--ignore-case") || !strings.Contains(script, "--exclude='*.[sS][aA][v]'") {
		t.Fatalf("running directory archive does not use a portable case-insensitive .sav exclusion: %s", script)
	}
}

func backupFilesystem(t *testing.T) (*kubeOrchestrator, *backupFilesystemRunner) {
	t.Helper()
	r := &backupFilesystemRunner{root: t.TempDir()}
	return &kubeOrchestrator{runner: r, kubectl: "unused"}, r
}

func writeBackupFixture(t *testing.T, root, name, data string) {
	t.Helper()
	file := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestBackupFilesystemPaths(t *testing.T) {
	k, r := backupFilesystem(t)
	writeBackupFixture(t, r.root, "config", "extensionless")
	writeBackupFixture(t, r.root, "saves/World with spaces.sav.backup", "world")
	writeBackupFixture(t, r.root, "saves/Legacy.bak", "wrong convention")
	writeBackupFixture(t, r.root, "saves/Generic.backup", "not a world backup")
	writeBackupFixture(t, r.root, "saves/World.sav", "live save must not be read")
	for _, forbidden := range []string{"saves/Legacy.bak", "saves/Generic.backup", "saves/World.sav"} {
		if _, _, err := k.resolveBackupPath(context.Background(), backupKubeTarget(), forbidden, BackupItemFile, BackupServerRunning); err == nil {
			t.Fatalf("running collection accepted %s", forbidden)
		}
	}
	if _, _, err := k.resolveBackupPath(context.Background(), backupKubeTarget(), "saves/World with spaces.sav.backup", BackupItemFile, BackupServerStopped); err == nil {
		t.Fatal("stopped collection accepted the live backup instead of the flat save")
	}
	for _, relative := range []string{"config", "saves"} {
		_, resolved, err := k.resolveBackupPath(context.Background(), backupKubeTarget(), relative, BackupItemFile, BackupServerRunning)
		want := relative
		if relative == "saves" {
			want += "/World with spaces.sav.backup"
		}
		if err != nil || resolved != want {
			t.Fatalf("resolve %s = %s, %v", relative, resolved, err)
		}
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(r.root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("absent", filepath.Join(r.root, "dangling")); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{"escape/missing.sav.backup", "dangling", "saves/*.sav.backup", "config/child"} {
		_, _, err := k.resolveBackupPath(context.Background(), backupKubeTarget(), relative, BackupItemFile, BackupServerRunning)
		if err == nil || errors.Is(err, errBackupSourceMissing) {
			t.Fatalf("unsafe %s returned %v", relative, err)
		}
	}
	_, _, err := k.resolveBackupPath(context.Background(), backupKubeTarget(), "absent/World.sav.backup", BackupItemFile, BackupServerRunning)
	if !errors.Is(err, errBackupSourceMissing) {
		t.Fatalf("missing source = %v", err)
	}
}

func TestBackupFilesystemRejectsRunningSAVBeforeReading(t *testing.T) {
	k, r := backupFilesystem(t)
	writeBackupFixture(t, r.root, "saves/World.SAV", "must not be read")
	_, _, err := k.captureBackupItem(context.Background(), backupKubeTarget(), BackupItemSpec{Name: "world", Kind: BackupItemDirectory}, BackupSourceRule{Path: "saves"}, BackupServerRunning, t.TempDir(), 10240)
	if err == nil || r.reads != 0 || r.streams != 0 {
		t.Fatalf("unsafe capture err=%v reads=%d streams=%d", err, r.reads, r.streams)
	}
}

func TestBackupFilesystemCapture(t *testing.T) {
	for _, test := range []struct {
		name      string
		directory bool
		transform func([]byte) []byte
		wantErr   bool
	}{
		{name: "file"},
		{name: "directory", directory: true},
		{name: "truncated file", transform: func(b []byte) []byte { return b[:len(b)-1] }, wantErr: true},
		{name: "corrupted file", transform: func(b []byte) []byte { b[0] ^= 1; return b }, wantErr: true},
		{name: "corrupted directory", directory: true, transform: func(b []byte) []byte {
			for i := 0; i+5 < len(b); i++ {
				if string(b[i:i+5]) == "world" {
					b[i] = 'X'
					break
				}
			}
			return b
		}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			k, r := backupFilesystem(t)
			writeBackupFixture(t, r.root, "saves/World.sav.backup", "world")
			r.transform = test.transform
			kind, relative := BackupItemFile, "saves/World.sav.backup"
			if test.directory {
				kind, relative = BackupItemDirectory, "saves"
			}
			start := time.Now()
			_, file, err := k.captureBackupItem(context.Background(), backupKubeTarget(), BackupItemSpec{Name: "world", Kind: kind}, BackupSourceRule{Path: relative}, BackupServerRunning, t.TempDir(), 102400)
			if file != nil {
				defer file.Close()
			}
			if (err != nil) != test.wantErr {
				t.Fatalf("capture error = %v", err)
			}
			if time.Since(start) < backupStableInterval {
				t.Fatal("missing stability interval")
			}
		})
	}
}

func TestBackupFilesystemDetectsPrecopyMutation(t *testing.T) {
	for _, mode := range []string{"content", "nanoseconds"} {
		t.Run(mode, func(t *testing.T) {
			k, r := backupFilesystem(t)
			writeBackupFixture(t, r.root, "World.sav.backup", "world")
			file := filepath.Join(r.root, "World.sav.backup")
			stamp := time.Unix(1700000000, 100)
			if err := os.Chtimes(file, stamp, stamp); err != nil {
				t.Fatal(err)
			}
			r.afterRead = func(n int) {
				if n == 1 {
					if mode == "content" {
						writeBackupFixture(t, r.root, "World.sav.backup", "other")
					} else {
						stamp = stamp.Add(time.Nanosecond)
					}
					if err := os.Chtimes(file, stamp, stamp); err != nil {
						t.Fatal(err)
					}
				}
			}
			_, _, err := k.captureBackupItem(context.Background(), backupKubeTarget(), BackupItemSpec{Name: "world", Kind: BackupItemFile}, BackupSourceRule{Path: "World.sav.backup"}, BackupServerRunning, t.TempDir(), 1024)
			if err == nil || r.streams != 0 {
				t.Fatalf("mutation err=%v streams=%d", err, r.streams)
			}
		})
	}
}

func TestBackupCaptureRejectsArchiveLinks(t *testing.T) {
	k, r := backupFilesystem(t)
	writeBackupFixture(t, r.root, "saves/World.sav.backup", "world")
	r.transform = func(_ []byte) []byte {
		var b bytes.Buffer
		w := tar.NewWriter(&b)
		if err := w.WriteHeader(&tar.Header{Name: "saves/escape", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	_, _, err := k.captureBackupItem(context.Background(), backupKubeTarget(), BackupItemSpec{Name: "world", Kind: BackupItemDirectory}, BackupSourceRule{Path: "saves"}, BackupServerRunning, t.TempDir(), 102400)
	if err == nil || !strings.Contains(err.Error(), "unsafe captured archive") {
		t.Fatalf("archive link = %v", err)
	}
}
