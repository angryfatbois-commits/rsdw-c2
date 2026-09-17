package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

type deletionParserRunner struct {
	response string
	calls    []string
}

func (r *deletionParserRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	return []byte(r.response), nil
}

func TestDeletionConfigMapPlainData(t *testing.T) {
	const live = `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"world-config","namespace":"games","uid":"config-uid"},"data":{"server.ini":"[Server]\nName=Living World\n","enabled":"true"}}`
	runner := &deletionParserRunner{response: live}
	k := &kubeOrchestrator{runner: runner, kubectl: "kubectl"}
	got, err := k.deletionGet(context.Background(), "ConfigMap", "games", "world-config")
	if err != nil || got.Data["server.ini"] != "[Server]\nName=Living World\n" {
		t.Fatalf("plain ConfigMap get: %+v, %v", got, err)
	}
	runner.response = `{"items":[` + live + `,{"apiVersion":"v1","kind":"Secret","metadata":{"name":"world-api","namespace":"games","uid":"secret-uid"},"data":{"token":"c2VjcmV0"}}]}`
	items, err := k.deletionList(context.Background(), "games", "secrets,deployments,services,configmaps,persistentvolumeclaims")
	if err != nil || len(items) != 2 || items[0].Data["server.ini"] != "[Server]\nName=Living World\n" || items[1].Data["token"] != "c2VjcmV0" {
		t.Fatalf("mixed resource list: %+v, %v", items, err)
	}
	const manifest = `apiVersion: v1
kind: ConfigMap
metadata:
  name: world-config
data:
  server.ini: |
    [Server]
    Name=Living World
  enabled: "true"
`
	resources, err := manifestResources(storedRelease{Namespace: "games", Manifest: manifest})
	if err != nil || len(resources) != 1 || resources[0].Data["server.ini"] != "[Server]\nName=Living World\n" {
		t.Fatalf("stored YAML ConfigMap: %+v, %v", resources, err)
	}
	if len(runner.calls) != 2 || runner.calls[0] != "kubectl -n games get ConfigMap world-config --ignore-not-found -o json" || runner.calls[1] != "kubectl -n games get secrets,deployments,services,configmaps,persistentvolumeclaims -o json" {
		t.Fatalf("unexpected commands: %v", runner.calls)
	}
}

func TestDeletionHelmSecretEncoding(t *testing.T) {
	release := storedRelease{Name: "world", Namespace: "games", Version: 1, Manifest: "plain manifest"}
	data, err := json.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	inner := base64.StdEncoding.EncodeToString(compressed.Bytes())
	outer := base64.StdEncoding.EncodeToString([]byte(inner))
	runner := &deletionParserRunner{response: fmt.Sprintf(`{"items":[{"kind":"Secret","metadata":{"name":"sh.helm.release.v1.world.v1","namespace":"games","uid":"helm-uid"},"data":{"release":%q}}]}`, outer)}
	k := &kubeOrchestrator{runner: runner, kubectl: "kubectl"}
	got, storage, err := k.deletionRelease(context.Background(), "games", "world")
	if err != nil || got.Name != "world" || got.Namespace != "games" || got.Version != 1 || got.Manifest != "plain manifest" || storage.UID != "helm-uid" {
		t.Fatalf("Helm double base64/gzip decoding: %+v %+v %v", got, storage, err)
	}
}
