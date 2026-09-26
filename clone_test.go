package main

import (
	"net/http"
	"testing"
)

func cloneValidBody() map[string]any {
	return map[string]any{
		"serverName":    "Copy World",
		"ownerName":     "Admin",
		"ownerId":       "0123456789abcdef0123456789abcdef",
		"confirmCreate": true,
	}
}

func TestCloneFromStoppedServerDemo(t *testing.T) {
	app := backupTestApp(t)
	response := backupJSONRequest(t, app, http.MethodPost, "/api/servers", map[string]any{
		"name": "Clone source", "ownerId": "0123456789abcdef0123456789abcdef", "maxPlayers": 4,
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("create source = %d: %s", response.Code, response.Body.String())
	}
	source := decodeBackupResponse[Server](t, response)
	stop := backupJSONRequest(t, app, http.MethodPost, "/api/servers/"+source.ID+"/actions/stop", map[string]any{"confirm": true})
	if stop.Code != http.StatusOK {
		t.Fatalf("stop source = %d: %s", stop.Code, stop.Body.String())
	}

	clone := backupJSONRequest(t, app, http.MethodPost, "/api/servers/"+source.ID+"/clone", cloneValidBody())
	if clone.Code != http.StatusCreated {
		t.Fatalf("clone from stopped server = %d: %s", clone.Code, clone.Body.String())
	}
	created := decodeBackupResponse[Server](t, clone)
	if created.ID == source.ID {
		t.Fatal("clone reused the source server ID")
	}
	if created.Status != StatusStopped {
		t.Fatalf("cloned server status = %s, want stopped", created.Status)
	}
	if created.AdminIDs != "" {
		t.Fatal("clone must not copy admin IDs")
	}
	if created.ServerPassword != "" || created.AdminPassword != "" {
		t.Fatal("clone must not copy server or admin passwords")
	}

	reloaded := app.store.Snapshot().Servers[source.ID]
	if reloaded.Status != StatusStopped {
		t.Fatalf("source server status changed to %s after clone", reloaded.Status)
	}
}

func TestCloneRejectsRunningSource(t *testing.T) {
	app := backupTestApp(t)
	response := backupJSONRequest(t, app, http.MethodPost, "/api/servers", map[string]any{
		"name": "Online source", "ownerId": "0123456789abcdef0123456789abcdef", "maxPlayers": 4,
	})
	source := decodeBackupResponse[Server](t, response)
	if source.Status != StatusOnline {
		t.Fatalf("demo created server status = %s, want online", source.Status)
	}
	clone := backupJSONRequest(t, app, http.MethodPost, "/api/servers/"+source.ID+"/clone", cloneValidBody())
	if clone.Code != http.StatusConflict {
		t.Fatalf("clone from online server = %d: %s, want 409", clone.Code, clone.Body.String())
	}
}

func TestCloneRejectsDeletingSource(t *testing.T) {
	app := backupTestApp(t)
	response := backupJSONRequest(t, app, http.MethodPost, "/api/servers", map[string]any{
		"name": "Deleting source", "ownerId": "0123456789abcdef0123456789abcdef", "maxPlayers": 4,
	})
	source := decodeBackupResponse[Server](t, response)
	if err := app.store.Update(func(state *State) error {
		server := state.Servers[source.ID]
		server.Status = StatusStopped
		state.Servers[source.ID] = server
		if state.Deletions == nil {
			state.Deletions = map[string]deletionRecord{}
		}
		state.Deletions[source.ID] = deletionRecord{ServerID: source.ID, Mode: purgeWorld}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	clone := backupJSONRequest(t, app, http.MethodPost, "/api/servers/"+source.ID+"/clone", cloneValidBody())
	if clone.Code != http.StatusConflict {
		t.Fatalf("clone from deleting server = %d: %s, want 409", clone.Code, clone.Body.String())
	}
}

func TestCloneSourceNotFound(t *testing.T) {
	app := backupTestApp(t)
	clone := backupJSONRequest(t, app, http.MethodPost, "/api/servers/does-not-exist/clone", cloneValidBody())
	if clone.Code != http.StatusNotFound {
		t.Fatalf("clone from missing source = %d: %s, want 404", clone.Code, clone.Body.String())
	}
}

func TestCloneFromRetainedWorldDemo(t *testing.T) {
	app := backupTestApp(t)
	if err := app.store.Update(func(state *State) error {
		if state.Deletions == nil {
			state.Deletions = map[string]deletionRecord{}
		}
		state.Deletions["old-world"] = deletionRecord{
			ServerID: "old-world", WorldLabel: "Old World", Namespace: "dragonwilds", Mode: keepWorld, Completed: true,
			Plan: deletionPlan{World: []resourceIdentity{{Kind: "PersistentVolumeClaim", Namespace: "dragonwilds", Name: "old-world-rsdragonwilds", UID: "pvc-uid"}}},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	clone := backupJSONRequest(t, app, http.MethodPost, "/api/servers/old-world/clone", cloneValidBody())
	if clone.Code != http.StatusCreated {
		t.Fatalf("clone from retained world = %d: %s", clone.Code, clone.Body.String())
	}
	created := decodeBackupResponse[Server](t, clone)
	if created.Status != StatusStopped {
		t.Fatalf("cloned server status = %s, want stopped", created.Status)
	}
	if created.ID == "old-world" {
		t.Fatal("clone reused the retained server ID")
	}
}

func TestCloneRejectsPurgedDeletion(t *testing.T) {
	app := backupTestApp(t)
	if err := app.store.Update(func(state *State) error {
		if state.Deletions == nil {
			state.Deletions = map[string]deletionRecord{}
		}
		state.Deletions["purged-world"] = deletionRecord{ServerID: "purged-world", Mode: purgeWorld, Completed: true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	clone := backupJSONRequest(t, app, http.MethodPost, "/api/servers/purged-world/clone", cloneValidBody())
	if clone.Code != http.StatusConflict {
		t.Fatalf("clone from purged deletion = %d: %s, want 409", clone.Code, clone.Body.String())
	}
}

func TestCloneRejectsIncompleteDeletion(t *testing.T) {
	app := backupTestApp(t)
	if err := app.store.Update(func(state *State) error {
		if state.Deletions == nil {
			state.Deletions = map[string]deletionRecord{}
		}
		state.Deletions["in-progress-world"] = deletionRecord{
			ServerID: "in-progress-world", Mode: keepWorld, Completed: false,
			Plan: deletionPlan{World: []resourceIdentity{{Kind: "PersistentVolumeClaim", Namespace: "dragonwilds", Name: "in-progress-world-rsdragonwilds", UID: "pvc-uid"}}},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	clone := backupJSONRequest(t, app, http.MethodPost, "/api/servers/in-progress-world/clone", cloneValidBody())
	if clone.Code != http.StatusConflict {
		t.Fatalf("clone from in-progress deletion = %d: %s, want 409", clone.Code, clone.Body.String())
	}
}

func TestCloneRejectsAmbiguousRetainedPlan(t *testing.T) {
	app := backupTestApp(t)
	if err := app.store.Update(func(state *State) error {
		if state.Deletions == nil {
			state.Deletions = map[string]deletionRecord{}
		}
		state.Deletions["ambiguous-world"] = deletionRecord{ServerID: "ambiguous-world", Mode: keepWorld, Completed: true, Plan: deletionPlan{World: []resourceIdentity{
			{Kind: "PersistentVolumeClaim", Namespace: "dragonwilds", Name: "ambiguous-world-a", UID: "pvc-uid-a"},
			{Kind: "PersistentVolumeClaim", Namespace: "dragonwilds", Name: "ambiguous-world-b", UID: "pvc-uid-b"},
		}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	clone := backupJSONRequest(t, app, http.MethodPost, "/api/servers/ambiguous-world/clone", cloneValidBody())
	if clone.Code != http.StatusConflict {
		t.Fatalf("clone from empty-plan deletion = %d: %s, want 409", clone.Code, clone.Body.String())
	}
}

func TestCloneRequestValidation(t *testing.T) {
	app := backupTestApp(t)
	response := backupJSONRequest(t, app, http.MethodPost, "/api/servers", map[string]any{
		"name": "Validation source", "ownerId": "0123456789abcdef0123456789abcdef", "maxPlayers": 4,
	})
	source := decodeBackupResponse[Server](t, response)
	stop := backupJSONRequest(t, app, http.MethodPost, "/api/servers/"+source.ID+"/actions/stop", map[string]any{"confirm": true})
	if stop.Code != http.StatusOK {
		t.Fatalf("stop source = %d: %s", stop.Code, stop.Body.String())
	}
	cases := []struct {
		name string
		body map[string]any
	}{
		{"missing confirmCreate", map[string]any{"serverName": "X", "ownerName": "Admin", "ownerId": "0123456789abcdef0123456789abcdef"}},
		{"empty serverName", map[string]any{"serverName": "", "ownerName": "Admin", "ownerId": "0123456789abcdef0123456789abcdef", "confirmCreate": true}},
		{"empty ownerName", map[string]any{"serverName": "X", "ownerName": "", "ownerId": "0123456789abcdef0123456789abcdef", "confirmCreate": true}},
		{"invalid ownerId", map[string]any{"serverName": "X", "ownerName": "Admin", "ownerId": "not-hex", "confirmCreate": true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clone := backupJSONRequest(t, app, http.MethodPost, "/api/servers/"+source.ID+"/clone", c.body)
			if clone.Code != http.StatusBadRequest {
				t.Fatalf("%s = %d: %s, want 400", c.name, clone.Code, clone.Body.String())
			}
		})
	}
}
