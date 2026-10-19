package config

import (
	"strings"
	"testing"
)

// task_file is boot material like instructions_file: confined to the
// instance root and outside the tree, so no agent picks a worker's task.
func TestWorkerTaskFileIsBootMaterial(t *testing.T) {
	const head = "name: demo\nbackend: orbstack\ntree: ws\nbroker:\n  llm_auth: subscription\nmanager: {}\nworkers:\n  - name: w\n    dir: w\n"
	for label, tc := range map[string]struct{ body, want string }{
		"traversal": {head + "    task_file: ../task.md\n", "task_file"},
		"absolute":  {head + "    task_file: /etc/task.md\n", "task_file"},
		"in tree":   {head + "    task_file: ws/w/task.md\n", "inside the mounted tree"},
	} {
		_, err := LoadNoHostChecks(writeConfig(t, tc.body))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: got %v, want %q", label, err, tc.want)
		}
	}
	app, err := LoadNoHostChecks(writeConfig(t, head+"    task_file: tasks/w.md\n    autostart: true\n  - name: bare\n    dir: bare\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := app.WorkerTaskPath(app.Workers[0]); !strings.HasSuffix(got, "/tasks/w.md") || !app.Workers[0].Autostart {
		t.Fatalf("task path %q autostart %v", got, app.Workers[0].Autostart)
	}
	if got := app.WorkerTaskPath(app.Workers[1]); got != "" || app.Workers[1].Autostart {
		t.Fatalf("unset: %q %v", got, app.Workers[1].Autostart)
	}
}
