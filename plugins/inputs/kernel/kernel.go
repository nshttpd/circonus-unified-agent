//go:build linux
// +build linux

package kernel

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/circonus-labs/circonus-unified-agent/cua"
	"github.com/circonus-labs/circonus-unified-agent/plugins/inputs"
)

// /proc/stat file line prefixes to gather stats on:
var (
	interrupts      = []byte("intr")
	contextSwitches = []byte("ctxt")
	processesForked = []byte("processes")
	diskPages       = []byte("page")
	bootTime        = []byte("btime")
)

type Kernel struct {
	statFile        string
	entropyStatFile string
}

func (k *Kernel) Description() string {
	return "Get kernel statistics from /proc/stat"
}

func (k *Kernel) SampleConfig() string { return "" }

func (k *Kernel) Gather(ctx context.Context, acc cua.Accumulator) error {

	data, err := k.getProcStat()
	if err != nil {
		return err
	}

	entropyData, err := os.ReadFile(k.entropyStatFile)
	if err != nil {
		return fmt.Errorf("readfile: %w", err)
	}

	entropyString := string(entropyData)
	entropyValue, err := strconv.ParseInt(strings.TrimSpace(entropyString), 10, 64)
	if err != nil {
		return fmt.Errorf("kernel parseint %s: %w", strings.TrimSpace(entropyString), err)
	}

	fields := make(map[string]interface{})

	fields["entropy_avail"] = entropyValue

	dataFields := bytes.Fields(data)
	for i, field := range dataFields {
		switch {
		case bytes.Equal(field, interrupts):
			m, err := strconv.ParseInt(string(dataFields[i+1]), 10, 64)
			if err != nil {
				return fmt.Errorf("kernel parseint interrupts %s: %w", string(dataFields[i+1]), err)
			}
			fields["interrupts"] = m
		case bytes.Equal(field, contextSwitches):
			m, err := strconv.ParseInt(string(dataFields[i+1]), 10, 64)
			if err != nil {
				return fmt.Errorf("kernel parseint context_switches %s: %w", string(dataFields[i+1]), err)
			}
			fields["context_switches"] = m
		case bytes.Equal(field, processesForked):
			m, err := strconv.ParseInt(string(dataFields[i+1]), 10, 64)
			if err != nil {
				return fmt.Errorf("kernel parseint processes_forked %s: %w", string(dataFields[i+1]), err)
			}
			fields["processes_forked"] = m
		case bytes.Equal(field, bootTime):
			m, err := strconv.ParseInt(string(dataFields[i+1]), 10, 64)
			if err != nil {
				return fmt.Errorf("kernel parseint boot_time %s: %w", string(dataFields[i+1]), err)
			}
			fields["boot_time"] = m
		case bytes.Equal(field, diskPages):
			in, err := strconv.ParseInt(string(dataFields[i+1]), 10, 64)
			if err != nil {
				return fmt.Errorf("kernel parseint disk_pages_in %s: %w", string(dataFields[i+1]), err)
			}
			out, err := strconv.ParseInt(string(dataFields[i+2]), 10, 64)
			if err != nil {
				return fmt.Errorf("kernel parseint disk_pages_out %s: %w", string(dataFields[i+2]), err)
			}
			fields["disk_pages_in"] = in
			fields["disk_pages_out"] = out
		}
	}

	acc.AddCounter("kernel", fields, map[string]string{})

	return nil
}

func (k *Kernel) getProcStat() ([]byte, error) {
	if _, err := os.Stat(k.statFile); os.IsNotExist(err) {
		return nil, fmt.Errorf("kernel: %s does not exist", k.statFile)
	} else if err != nil {
		return nil, fmt.Errorf("kernel stat (%s): %w", k.statFile, err)
	}

	data, err := os.ReadFile(k.statFile)
	if err != nil {
		return nil, fmt.Errorf("kernel readfile (%s): %w", k.statFile, err)
	}

	return data, nil
}

func init() {
	inputs.Add("kernel", func() cua.Input {
		return &Kernel{
			statFile:        "/proc/stat",
			entropyStatFile: "/proc/sys/kernel/random/entropy_avail",
		}
	})
}
