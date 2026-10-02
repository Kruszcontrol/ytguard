package core

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"ytguard"
	"ytguard/internal/filterlist"
	"ytguard/internal/store"
)

// ListRefreshInterval is how often subscribed lists are checked for updates.
const ListRefreshInterval = 24 * time.Hour

// DefaultCatalogURL is the recommended-lists catalog for this build: the
// ytguard-lists repository next to the app's own repository.
func DefaultCatalogURL() string {
	owner, _, ok := strings.Cut(ytguard.Repo, "/")
	if !ok || owner == "" {
		return ""
	}
	return "https://raw.githubusercontent.com/" + owner + "/ytguard-lists/main/catalog.json"
}

// CatalogURL is the configured catalog address.
func (a *App) CatalogURL() string {
	if s, _ := a.St.Settings(); s.ListCatalogURL != "" {
		return s.ListCatalogURL
	}
	return DefaultCatalogURL()
}

var (
	catalogMu    sync.Mutex
	catalogCache filterlist.Catalog
	catalogURL   string
	catalogAt    time.Time
	catalogErr   error
)

// Catalog returns the recommended lists (cached for an hour).
func (a *App) Catalog(ctx context.Context) (filterlist.Catalog, error) {
	u := a.CatalogURL()
	if u == "" {
		return filterlist.Catalog{}, errors.New("no catalog address configured")
	}
	catalogMu.Lock()
	defer catalogMu.Unlock()
	if u == catalogURL && time.Since(catalogAt) < time.Hour {
		return catalogCache, catalogErr
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	catalogCache, catalogErr = filterlist.FetchCatalog(ctx, u)
	catalogURL, catalogAt = u, time.Now()
	return catalogCache, catalogErr
}

// Subscribe adds a list and downloads it. kids empty = all kids.
func (a *App) Subscribe(ctx context.Context, url string, kids []int64) (store.FilterList, error) {
	url = strings.TrimSpace(url)
	if err := filterlist.CheckURL(url); err != nil {
		return store.FilterList{}, err
	}
	if _, err := a.St.ListByURL(url); err == nil {
		return store.FilterList{}, errors.New("already subscribed to that list")
	}
	l := store.FilterList{URL: url, Kids: kids, Enabled: true}
	if err := a.St.AddList(&l); err != nil {
		return l, err
	}
	if err := a.RefreshList(ctx, l.ID, true); err != nil {
		_ = a.St.DeleteList(l.ID)
		return l, err
	}
	return a.St.List(l.ID)
}

// RefreshList downloads a list again. Without force, the server is asked
// whether it changed. On failure the previous copy is kept.
func (a *App) RefreshList(ctx context.Context, id int64, force bool) error {
	l, err := a.St.List(id)
	if err != nil {
		return err
	}
	etag, lm := l.ETag, l.LastModified
	if force {
		etag, lm = "", ""
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	now := a.Now().Unix()
	f, err := filterlist.Fetch(ctx, l.URL, etag, lm)
	if err != nil {
		_ = a.St.SetListChecked(id, now, err.Error())
		return err
	}
	if f.NotModified {
		return a.St.SetListChecked(id, now, "")
	}
	parsed, err := filterlist.Parse(f.Text)
	if err != nil {
		_ = a.St.SetListChecked(id, now, err.Error())
		return err
	}
	m := parsed.Meta
	l.Title, l.Description, l.Ages, l.Homepage, l.License, l.Version = m.Title, m.Description, m.Ages, m.Homepage, m.License, m.Version
	l.ETag, l.LastModified, l.FetchedAt, l.CheckedAt, l.Warnings = f.ETag, f.LastModified, now, now, parsed.Warnings
	if err := a.St.ReplaceListContent(l, parsed.Rules); err != nil {
		return err
	}
	slog.Info("filter list updated", "list", l.Title, "rules", len(parsed.Rules), "warnings", len(parsed.Warnings))
	return nil
}

// RefreshDueLists checks every list that hasn't been checked for a day.
func (a *App) RefreshDueLists(ctx context.Context) {
	ls, err := a.St.Lists()
	if err != nil {
		return
	}
	for _, l := range ls {
		if a.Now().Unix()-l.CheckedAt < int64(ListRefreshInterval/time.Second) {
			continue
		}
		if err := a.RefreshList(ctx, l.ID, false); err != nil {
			slog.Warn("filter list update", "list", firstNonEmpty(l.Title, l.URL), "err", err)
		}
	}
}

// RunListUpdater keeps subscriptions current.
func (a *App) RunListUpdater(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Minute):
		}
		a.RefreshDueLists(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ListSummary describes subscriptions for display.
func ListSummary(l store.FilterList, kids []store.Kid) string {
	if len(l.Kids) == 0 {
		return "All kids"
	}
	var names []string
	for _, k := range kids {
		if l.AppliesTo(k.ID) {
			names = append(names, k.Name)
		}
	}
	if len(names) == 0 {
		return "Nobody"
	}
	return strings.Join(names, ", ")
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
