package jobs

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// jobsFile is the filename written under DataDir.
const jobsFile = "jobs.jsonl"

// LoadHistory reads jobs.jsonl from dataDir and returns all Job records
// whose StartTime is within maxAge of now. Records that fail to decode
// are silently skipped. Returns an empty slice (not an error) when the
// file does not exist yet.
func LoadHistory(dataDir string, maxAge time.Duration) ([]Job, error) {
	path := filepath.Join(dataDir, jobsFile)
	f, err := os.Open(path) //nolint:gosec // path is constructed from controlled dataDir constant
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("jobs: open history: %w", err)
	}
	defer f.Close()

	cutoff := time.Now().UTC().Add(-maxAge)
	seen := map[string]int{} // uid -> index in out, for dedup (last write wins)
	var out []Job

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var j Job
		if err := json.Unmarshal(line, &j); err != nil {
			continue // skip malformed lines
		}
		if j.StartTime.Before(cutoff) {
			continue
		}
		if idx, exists := seen[j.UID]; exists {
			out[idx] = j // later entry supersedes earlier
		} else {
			seen[j.UID] = len(out)
			out = append(out, j)
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return out, fmt.Errorf("jobs: scan history: %w", err)
	}
	return out, nil
}
