package main

import (
	"bytes"
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBackupRoutesRejectViewersAndUnauthenticated(t *testing.T) {
	app, issuer := oidcTestApp(t)
	cookies, csrf := loginAs(t, app, issuer, "viewer")
	routes := [][2]string{
		{"GET", "/api/backups"}, {"GET", "/api/backups/runs"},
		{"GET", "/api/backups/runs/example/bundle"}, {"GET", "/api/backups/profiles"},
		{"GET", "/api/backups/profiles/dragonwilds-world-save/export"},
		{"POST", "/api/backups/runs"}, {"POST", "/api/backups/schedules"},
		{"POST", "/api/backups/profiles/import"},
		{"POST", "/api/backups/example/create-server"},
		{"POST", "/api/servers/example/restore"},
	}
	for _, route := range routes {
		t.Run(route[0]+route[1], func(t *testing.T) {
			if got := authRequest(app, route[0], route[1], cookies, app.auth.settings.Origin, csrf, "{}"); got.Code != 403 {
				t.Fatalf("viewer: %d %s", got.Code, got.Body.String())
			}
			if got := authRequest(app, route[0], route[1], nil, "", "", "{}"); got.Code != 401 {
				t.Fatalf("anonymous: %d %s", got.Code, got.Body.String())
			}
		})
	}
}

func TestBackupProfileCollisionsAndRevisionsPreserveSchedules(t *testing.T) {
	app := backupTestApp(t)
	profile := backupDefinitionWithItems()
	create := backupJSONRequest(t, app, "POST", "/api/backups/profiles", profile)
	if create.Code != 200 {
		t.Fatal(create.Code, create.Body.String())
	}
	profile.Name = "Accidental overwrite"
	if r := backupJSONRequest(t, app, "POST", "/api/backups/profiles", profile); r.Code != 409 {
		t.Fatal(r.Code, r.Body.String())
	}
	yaml, _ := ExportBackupDefinitionYAML(profile)
	if r := backupJSONRequest(t, app, "POST", "/api/backups/profiles/import", map[string]string{"yaml": string(yaml)}); r.Code != 409 {
		t.Fatal(r.Code, r.Body.String())
	}
	if r := backupJSONRequest(t, app, "PUT", "/api/backups/profiles/"+profile.ID, profile); r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	if r := backupJSONRequest(t, app, "PUT", "/api/backups/profiles/"+profile.ID, profile); r.Code != 409 {
		t.Fatal("stale revision accepted", r.Code)
	}
	_ = app.store.Update(func(s *State) error {
		s.BackupSchedules["schedule"] = BackupSchedule{ID: "schedule", DefinitionID: profile.ID}
		return nil
	})
	if r := backupJSONRequest(t, app, "DELETE", "/api/backups/profiles/"+profile.ID, nil); r.Code != 409 {
		t.Fatal("referenced profile deleted", r.Code)
	}
	if got := app.store.Snapshot().BackupDefinitions[profile.ID]; got.Revision != 2 {
		t.Fatal(got)
	}
}

func TestBackupDefinitionRejectsUnsupportedSources(t *testing.T) {
	for _, source := range []string{"*.sav", "foo/[ab].sav.backup", "foo/../secret", "/etc/passwd", "foo\nbar", ".c2-restore/incoming/file"} {
		profile := backupDefinitionWithItems()
		profile.Items[0].Sources[0].Path = source
		if err := profile.Validate(); err == nil {
			t.Errorf("accepted %q", source)
		}
	}
	profile := backupDefinitionWithItems()
	profile.ServerType = "valheim"
	if err := profile.Validate(); err == nil {
		t.Fatal("unsupported collector capability accepted")
	}
	profile = backupDefinitionWithItems()
	profile.Items[0].Sources = profile.Items[0].Sources[1:]
	data, err := ExportBackupDefinitionYAML(profile)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ImportBackupDefinitionYAML(data)
	if err != nil || !reflect.DeepEqual(got, profile) {
		t.Fatal("stopped-only round trip", got, err)
	}
}

func TestRestoreWritesBytesAndRequiresDestructiveConfirmation(t *testing.T) {
	app := backupTestApp(t)
	server := app.store.Snapshot().Servers["scuffedtards"]
	server.Status = StatusStopped
	server.WorldName = "Restored World"
	_ = app.store.Update(func(s *State) error { s.Servers[server.ID] = server; return nil })
	files := []restoreFile{{Path: backupWorldSaveRoot + "/uploaded.SAV", Data: []byte("first save")}, {Path: "config/DedicatedServer.ini", Data: []byte("config bytes")}}
	if err := app.restoreFiles(context.Background(), server, files, false); err != nil {
		t.Fatal(err)
	}
	world := filepath.Join(app.backups.root, ".demo-worlds", server.ID, backupWorldSaveRoot, "Restored World.sav")
	if got, err := os.ReadFile(world); err != nil || string(got) != "first save" {
		t.Fatal(string(got), err)
	}
	files[0].Data = []byte("second save")
	if err := app.restoreFiles(context.Background(), server, files, false); !errors.Is(err, errRestoreConfirmation) {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(world); string(got) != "first save" {
		t.Fatal("unconfirmed restore changed world")
	}
	if err := app.restoreFiles(context.Background(), server, files, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(world); string(got) != "second save" {
		t.Fatal("confirmed restore did not replace bytes")
	}
	if got, err := os.ReadFile(filepath.Join(app.backups.root, ".demo-worlds", server.ID, "config/DedicatedServer.ini")); err != nil || string(got) != "config bytes" {
		t.Fatal(string(got), err)
	}
}

func TestTrackedRestoreChecksBundleAndRestoresContent(t *testing.T) {
	app := backupTestApp(t)
	response := backupJSONRequest(t, app, "POST", "/api/backups/runs", backupRunRequest{ServerID: "scuffedtards", DefinitionID: "dragonwilds-world-save", BackendID: "local", Acknowledge: true})
	if response.Code != 201 {
		t.Fatal(response.Code, response.Body.String())
	}
	run := decodeBackupResponse[BackupRun](t, response)
	files, err := app.backupRestoreFiles(context.Background(), run.ManifestID)
	defer cleanupRestoreFiles(files)
	if err != nil || len(files) != 1 || !strings.HasSuffix(files[0].Path, ".sav") {
		t.Fatal(files, err)
	}
	data, err := restoreFileBytes(files[0])
	if err != nil || !bytes.Contains(data, []byte("demo world")) {
		t.Fatal(string(data), err)
	}
	_ = app.store.Update(func(s *State) error {
		m := s.BackupManifests[run.ManifestID]
		m.Items[0].SHA256 = strings.Repeat("0", 64)
		s.BackupManifests[m.ID] = m
		return nil
	})
	if _, err := app.backupRestoreFiles(context.Background(), run.ManifestID); err == nil {
		t.Fatal("tampered manifest accepted")
	}
}

func TestRestoreMultipartRejectsUnsafeAndDuplicateParts(t *testing.T) {
	for _, name := range []string{"World.sav", "TEST-01.sav", "custom.SAV"} {
		if !validRestoreSaveName(name) {
			t.Errorf("rejected %s", name)
		}
	}
	for _, name := range []string{"../World.sav", "foo\\World.sav", "bad\n.sav", "-World.sav"} {
		if validRestoreSaveName(name) {
			t.Errorf("accepted %q", name)
		}
	}
	for _, duplicate := range []string{"request", "save", "truncated"} {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		request, _ := writer.CreateFormField("request")
		request.Write([]byte(`{"source":"custom","confirmEmpty":true}`))
		file, _ := writer.CreateFormFile("save", "World.sav")
		file.Write([]byte("world"))
		if duplicate == "request" {
			part, _ := writer.CreateFormField("request")
			part.Write([]byte(`{}`))
		}
		if duplicate == "save" {
			part, _ := writer.CreateFormFile("save", "extra.sav")
			part.Write([]byte("extra"))
		}
		if duplicate != "truncated" {
			writer.Close()
		}
		r := httptest.NewRequest("POST", "/", &body)
		r.Header.Set("Content-Type", writer.FormDataContentType())
		var req restoreRequest
		var filename string
		var data []byte
		if err := readRestoreMultipart(httptest.NewRecorder(), r, &req, &filename, &data); err == nil {
			t.Errorf("accepted %s", duplicate)
		}
	}
}

func TestBackupLifecycleExclusion(t *testing.T) {
	app := backupTestApp(t)
	app.lifecycleMu.Lock()
	defer app.lifecycleMu.Unlock()
	for _, endpoint := range []string{"/api/backups/runs", "/api/servers/scuffedtards/restore"} {
		if response := backupJSONRequest(t, app, http.MethodPost, endpoint, map[string]bool{"acknowledge": true}); response.Code != http.StatusConflict {
			t.Fatal(endpoint, response.Code)
		}
	}
}
