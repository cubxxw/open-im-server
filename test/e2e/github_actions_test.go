package e2e

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestGithubTokenNotEmpty verifies that the "github-token" used in the GitHub Actions workflow
// is not empty when the "dessant/lock-threads@v4" action is run. It does this by checking the
// value of the "GITHUB_TOKEN" environment variable, which should be the same as the "github-token".
func TestGithubTokenNotEmpty(t *testing.T) {
	githubToken := os.Getenv("GITHUB_TOKEN")

	assert.NotEmpty(t, githubToken, "The GITHUB_TOKEN environment variable should not be empty")
}
