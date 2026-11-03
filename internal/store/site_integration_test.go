//go:build integration

package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestSiteSettingsPostgresLifecycle(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	repository, err := Open(ctx, Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if err = repository.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	user := usageSummaryUser(t, ctx, repository, "site-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	actor := SiteActor{UserID: user.ID, RequestID: "site-request-0001"}
	initial, err := repository.SiteSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fields := SiteSettingsFields{CommunityEnabled: true, CommunityTitle: "交流群", CommunityQQGroup: "123456789", CommunityNote: "第一行\n第二行", RechargeEnabled: true, RechargeContact: "群主"}

	// A stale revision rolls back without advancing the singleton.
	if _, err = repository.UpdateSiteSettings(ctx, UpdateSiteSettingsParams{SiteSettingsFields: fields, ExpectedRevision: initial.Revision + 5, Actor: actor}); !errors.Is(err, ErrSiteSettingsConflict) {
		t.Fatalf("stale revision: %v", err)
	}
	if current, _ := repository.SiteSettings(ctx); current.Revision != initial.Revision {
		t.Fatalf("conflict advanced revision %d -> %d", initial.Revision, current.Revision)
	}

	// Two writers holding the same revision: exactly one wins.
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = repository.UpdateSiteSettings(ctx, UpdateSiteSettingsParams{SiteSettingsFields: fields, ExpectedRevision: initial.Revision, Actor: actor})
		}(i)
	}
	wg.Wait()
	if (results[0] == nil) == (results[1] == nil) || !(errors.Is(results[0], ErrSiteSettingsConflict) || errors.Is(results[1], ErrSiteSettingsConflict)) {
		t.Fatalf("concurrent results = %v", results)
	}
	saved, err := repository.SiteSettings(ctx)
	if err != nil || saved.Revision != initial.Revision+1 || saved.SiteSettingsFields != fields {
		t.Fatalf("saved = %+v, %v", saved, err)
	}

	data := []byte("normalized image bytes")
	sum := sha256.Sum256(data)
	image := SiteImage{SiteImageInfo: SiteImageInfo{Slot: SiteImageRechargeWeChat, ContentType: "image/png", SHA256: hex.EncodeToString(sum[:]), Width: 300, Height: 400}, Data: data}
	afterImage, err := repository.PutSiteImage(ctx, image, actor)
	if err != nil || afterImage.Revision != saved.Revision+1 || afterImage.Images[SiteImageRechargeWeChat].SHA256 != image.SHA256 || afterImage.SiteSettingsFields != fields {
		t.Fatalf("after image = %+v, %v", afterImage, err)
	}
	stored, err := repository.SiteImage(ctx, SiteImageRechargeWeChat)
	if err != nil || string(stored.Data) != string(data) || stored.Width != 300 || stored.Height != 400 {
		t.Fatalf("stored image = %+v, %v", stored.SiteImageInfo, err)
	}

	var audits int
	if err = repository.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE actor_user_id=$1 AND subject_type='site_settings' AND event_type IN ('site.settings_updated','site.image_updated') AND request_id='site-request-0001'`, user.ID).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("audits = %d, %v", audits, err)
	}

	// The schema rejects what validation would, even for out-of-band writes.
	for _, statement := range []string{
		`UPDATE site_settings SET community_join_url='http://qm.qq.com' WHERE singleton`,
		`UPDATE site_settings SET community_qq_group='12' WHERE singleton`,
		`INSERT INTO site_images(slot,content_type,data,sha256,width,height,updated_at) VALUES('banner','image/png','\x00','` + image.SHA256 + `',1,1,now())`,
		`UPDATE site_images SET content_type='image/svg+xml'`,
		`INSERT INTO site_settings(singleton) VALUES(false)`,
	} {
		if _, err := repository.db.ExecContext(ctx, statement); err == nil {
			t.Fatalf("constraint accepted %s", statement)
		}
	}

	if _, err = repository.DeleteSiteImage(ctx, SiteImageRechargeWeChat, actor); err != nil {
		t.Fatal(err)
	}
	before, _ := repository.SiteSettings(ctx)
	if _, err = repository.DeleteSiteImage(ctx, SiteImageRechargeWeChat, actor); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if after, _ := repository.SiteSettings(ctx); after.Revision != before.Revision || len(after.Images) != 0 {
		t.Fatalf("missing delete advanced revision or left images: %+v", after)
	}
	if _, err = repository.UpdateSiteSettings(ctx, UpdateSiteSettingsParams{ExpectedRevision: before.Revision, Actor: actor}); err != nil {
		t.Fatalf("reset: %v", err)
	}
}
