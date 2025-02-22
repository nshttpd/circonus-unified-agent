//go:build !windows
// +build !windows

// postfix doesn't aim for windows

package postfix

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/circonus-labs/circonus-unified-agent/cua"
	"github.com/circonus-labs/circonus-unified-agent/plugins/inputs"
)

const sampleConfig = `
  ## Postfix queue directory. If not provided, agent will try to use
  ## 'postconf -h queue_directory' to determine it.
  # queue_directory = "/var/spool/postfix"
`

const description = "Measure postfix queue statistics"

func getQueueDirectory() (string, error) {
	qd, err := exec.Command("postconf", "-h", "queue_directory").Output()
	if err != nil {
		return "", fmt.Errorf("exec: %w", err)
	}
	return strings.TrimSpace(string(qd)), nil
}

func qScan(path string, acc cua.Accumulator) (int64, int64, int64, error) {
	var length, size int64
	var oldest time.Time
	err := filepath.Walk(path, func(_ string, finfo os.FileInfo, err error) error {
		if err != nil {
			acc.AddError(fmt.Errorf("error scanning %s: %w", path, err))
			return nil
		}
		if finfo.IsDir() {
			return nil
		}

		length++
		size += finfo.Size()

		ctime := statCTime(finfo.Sys())
		if ctime.IsZero() {
			return nil
		}
		if oldest.IsZero() || ctime.Before(oldest) {
			oldest = ctime
		}
		return nil
	})
	if err != nil {
		return 0, 0, 0, fmt.Errorf("filepath walk: %w", err)
	}
	var age int64
	if !oldest.IsZero() {
		age = int64(time.Since(oldest) / time.Second)
	} else if length != 0 {
		// system doesn't support ctime
		age = -1
	}
	return length, size, age, nil
}

type Postfix struct {
	QueueDirectory string
}

func (p *Postfix) Gather(ctx context.Context, acc cua.Accumulator) error {
	if p.QueueDirectory == "" {
		var err error
		p.QueueDirectory, err = getQueueDirectory()
		if err != nil {
			return fmt.Errorf("unable to determine queue directory: %w", err)
		}
	}

	for _, q := range []string{"active", "hold", "incoming", "maildrop", "deferred"} {
		length, size, age, err := qScan(filepath.Join(p.QueueDirectory, q), acc)
		if err != nil {
			acc.AddError(fmt.Errorf("error scanning queue %s: %w", q, err))
			continue
		}
		fields := map[string]interface{}{"length": length, "size": size}
		if age != -1 {
			fields["age"] = age
		}
		acc.AddFields("postfix_queue", fields, map[string]string{"queue": q})
	}

	return nil
}

func (p *Postfix) SampleConfig() string {
	return sampleConfig
}

func (p *Postfix) Description() string {
	return description
}

func init() {
	inputs.Add("postfix", func() cua.Input {
		return &Postfix{
			QueueDirectory: "/var/spool/postfix",
		}
	})
}
