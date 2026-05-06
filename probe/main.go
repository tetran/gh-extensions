package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	gh "github.com/cli/go-gh/v2"
	"github.com/cli/go-gh/v2/pkg/api"
)

func main() {
	fmt.Println("=== Probe 1: api.NewRESTClient -> GET /user ===")
	if err := probeREST(); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("\n=== Probe 2: gh pr view --json via gh.Exec ===")
	if err := probeGhExec(); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("\n=== Probe 3: HEAD resolution via os/exec git rev-parse ===")
	if err := probeHead(); err != nil {
		fmt.Fprintf(os.Stderr, "WARN: %v (probably outside a clone with main; OK for this probe)\n", err)
	}

	fmt.Println("\nAll probes finished.")
}

func probeREST() error {
	client, err := api.NewRESTClient(api.ClientOptions{})
	if err != nil {
		return fmt.Errorf("NewRESTClient: %w", err)
	}
	var user struct {
		Login string `json:"login"`
		ID    int    `json:"id"`
	}
	if err := client.Get("user", &user); err != nil {
		return fmt.Errorf("Get(user): %w", err)
	}
	fmt.Printf("login=%s id=%d\n", user.Login, user.ID)
	return nil
}

func probeGhExec() error {
	// cli/cli PR #1000 is a real merged PR; pick small, public.
	// cli/cli PR #13281 closes Issue #13280; merged. Used as a stable probe target.
	stdout, stderr, err := gh.Exec(
		"pr", "view", "13281",
		"--repo", "cli/cli",
		"--json", "number,state,mergeCommit,closingIssuesReferences,headRefName",
	)
	if err != nil {
		return fmt.Errorf("gh.Exec: %w (stderr=%s)", err, stderr.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		return fmt.Errorf("unmarshal: %w (raw=%s)", err, stdout.String())
	}
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	fmt.Printf("keys=%v\n", keys)
	if mc, ok := payload["mergeCommit"].(map[string]any); ok {
		fmt.Printf("mergeCommit.oid=%v\n", mc["oid"])
	}
	if cir, ok := payload["closingIssuesReferences"].([]any); ok {
		fmt.Printf("closingIssuesReferences len=%d\n", len(cir))
	}
	return nil
}

func probeHead() error {
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("git rev-parse: %w", err)
	}
	fmt.Printf("HEAD=%s\n", strings.TrimSpace(string(out)))
	return nil
}
