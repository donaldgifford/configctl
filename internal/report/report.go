package report

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

type Report struct {
	RunMode   string          `json:"runMode"`
	Timestamp string          `json:"timestamp"`
	Summary   Summary         `json:"summary"`
	Accounts  []AccountReport `json:"accounts"`
}

type Summary struct {
	TotalAccounts    int `json:"totalAccounts"`
	TotalRegions     int `json:"totalRegions"`
	RecordersEnabled int `json:"recordersEnabled"`
	RecordersDisabled int `json:"recordersDisabled"`
	RecordersAbsent  int `json:"recordersAbsent"`
	Errors           int `json:"errors"`
}

type AccountReport struct {
	AccountID       string         `json:"accountId"`
	IsManagementAcct bool          `json:"isManagementAccount"`
	Regions         []RegionReport `json:"regions"`
}

type RegionReport struct {
	Region        string  `json:"region"`
	RecorderName  string  `json:"recorderName,omitempty"`
	WasEnabled    bool    `json:"wasEnabled"`
	CurrentStatus string  `json:"currentStatus"`
	Error         *string `json:"error"`
}

// Collector is a thread-safe accumulator for region results.
type Collector struct {
	mu      sync.Mutex
	results map[string][]RegionReport // accountID -> regions
	mgmtAcct string
}

func NewCollector() *Collector {
	return &Collector{
		results: make(map[string][]RegionReport),
	}
}

func (c *Collector) SetManagementAccount(accountID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mgmtAcct = accountID
}

func (c *Collector) Add(accountID string, result RegionReport) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.results[accountID] = append(c.results[accountID], result)
}

func (c *Collector) Build(dryRun bool, accountIDs []string) *Report {
	c.mu.Lock()
	defer c.mu.Unlock()

	mode := "live"
	if dryRun {
		mode = "dry-run"
	}

	r := &Report{
		RunMode:   mode,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Accounts:  make([]AccountReport, 0, len(accountIDs)),
	}

	uniqueAccounts := make(map[string]struct{})
	uniqueRegions := make(map[string]struct{})

	for _, acctID := range accountIDs {
		uniqueAccounts[acctID] = struct{}{}

		regions := c.results[acctID]
		acctReport := AccountReport{
			AccountID:       acctID,
			IsManagementAcct: acctID == c.mgmtAcct,
			Regions:         regions,
		}

		for _, reg := range regions {
			uniqueRegions[reg.Region] = struct{}{}

			if reg.Error != nil {
				r.Summary.Errors++
				continue
			}

			switch reg.CurrentStatus {
			case "enabled":
				r.Summary.RecordersEnabled++
			case "disabled", "stopped":
				r.Summary.RecordersDisabled++
			case "absent":
				r.Summary.RecordersAbsent++
			}
		}

		r.Accounts = append(r.Accounts, acctReport)
	}

	r.Summary.TotalAccounts = len(uniqueAccounts)
	r.Summary.TotalRegions = len(uniqueRegions)

	return r
}

func WriteJSON(report *Report, path string) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling report: %w", err)
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing report file: %w", err)
	}

	return nil
}
