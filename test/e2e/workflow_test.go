package e2e

import (
	"net/http"
	"testing"
	"os"
)

func TestGithubWorkflow(t *testing.T) {
	req, err := http.NewRequest("POST", "https://api.github.com/repos/openimsdk/open-im-server/actions/workflows/workflow.yml/dispatches", nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}

	req.Header.Add("Authorization", "Bearer "+os.Getenv("GITHUB_TOKEN"))
	req.Header.Add("Accept", "application/vnd.github.v3+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to send request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("Expected status code %d, got %d", http.StatusCreated, resp.StatusCode)
	}
}
