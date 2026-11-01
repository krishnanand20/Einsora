package docker

import (
	"encoding/csv"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

func parseNVIDIA(raw string) []Device {
	rows, err := csv.NewReader(strings.NewReader(raw)).ReadAll()
	if err != nil {
		return nil
	}
	var devices []Device
	seen := map[string]bool{}
	for _, row := range rows {
		if len(row) != 3 || len(devices) >= 64 {
			return nil
		}
		id, model := strings.TrimSpace(row[0]), strings.TrimSpace(row[1])
		if _, err := uuid.Parse(strings.TrimPrefix(id, "GPU-")); err != nil || !strings.HasPrefix(id, "GPU-") || seen[id] {
			return nil
		}
		mem, err := strconv.ParseInt(strings.TrimSpace(row[2]), 10, 64)
		if err != nil || mem <= 0 || mem > 64<<20 || len(model) > 100 {
			return nil
		}
		seen[id] = true
		devices = append(devices, Device{ID: id, Model: model, MemoryMiB: mem})
	}
	return devices
}
