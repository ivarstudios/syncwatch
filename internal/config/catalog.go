package config

import (
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
)

// CheckInfo describes one check and its defaults.
type CheckInfo struct {
	ID       string
	Title    string
	Category string
	Severity model.Severity
	Urgent   bool
	Debounce time.Duration
}

// Catalog lists every check in display order.
var Catalog = []CheckInfo{
	{"H1", "Server unreachable", model.CatHealth, model.Error, true, 5 * time.Minute},
	{"H2", "Servers not connected", model.CatHealth, model.Error, true, 15 * time.Minute},
	{"H3", "Server certificate changed", model.CatHealth, model.Error, true, 5 * time.Minute},
	{"H4", "Folder sync stopped", model.CatHealth, model.Error, true, 5 * time.Minute},
	{"H5", "Stuck sync", model.CatHealth, model.Warn, false, 5 * time.Minute},
	{"H6", "Paused for a long time", model.CatHealth, model.Warn, false, 5 * time.Minute},
	{"H7", "PC offline", model.CatHealth, model.Warn, false, 5 * time.Minute},
	{"H8", "Version mismatch", model.CatHealth, model.Warn, false, 5 * time.Minute},
	{"H9", "Pending device or folder", model.CatHealth, model.Warn, false, 5 * time.Minute},
	{"H10", "New sync conflicts", model.CatHealth, model.Warn, false, 5 * time.Minute},
	{"H11", "Connected through a relay", model.CatHealth, model.Warn, false, 5 * time.Minute},
	{"H12", "Relay connection lost", model.CatHealth, model.Warn, false, 5 * time.Minute},
	{"H13", "Certificate expires soon", model.CatHealth, model.Info, false, 5 * time.Minute},
	{"X1", "Same label, different folder IDs", model.CatCross, model.Warn, false, 5 * time.Minute},
	{"X2", "Folder in different share types", model.CatCross, model.Warn, false, 5 * time.Minute},
	{"S1", "Syncthing folder inside another", model.CatStructure, model.Error, true, 5 * time.Minute},
	{"S2", "Share root is a Syncthing folder", model.CatStructure, model.Error, true, 5 * time.Minute},
	{"S3", "Nested project folder", model.CatStructure, model.Error, true, 5 * time.Minute},
	{"S4", "Incorrect name", model.CatStructure, model.Warn, false, 5 * time.Minute},
	{"S5", "Incorrect share", model.CatStructure, model.Error, false, 5 * time.Minute},
	{"S6", "Duplicate name", model.CatStructure, model.Warn, false, 5 * time.Minute},
	{"S7", "Not in Syncthing", model.CatStructure, model.Info, false, 5 * time.Minute},
	{"S8", "Project folder deleted", model.CatStructure, model.Warn, true, 5 * time.Minute},
	{"C1", "Inactive project folder", model.CatCleanup, model.Info, false, 5 * time.Minute},
	{"C2", "Inactive device", model.CatCleanup, model.Info, false, 5 * time.Minute},
	{"C3", "Folder not used by device", model.CatCleanup, model.Info, false, 5 * time.Minute},
}

// CatalogByID indexes Catalog.
var CatalogByID = func() map[string]CheckInfo {
	m := make(map[string]CheckInfo, len(Catalog))
	for _, c := range Catalog {
		m[c.ID] = c
	}
	return m
}()
