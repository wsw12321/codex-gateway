package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	SiteImageCommunityQR    = "community_qr"
	SiteImageRechargeWeChat = "recharge_wechat"
	SiteImageRechargeAlipay = "recharge_alipay"

	SiteImageMaxBytes     = 8 << 20
	SiteImageMaxDimension = 4096
)

var (
	ErrSiteSettingsConflict = fmt.Errorf("%w: site settings revision changed", ErrConflict)
	ErrSiteCommunityContact = fmt.Errorf("%w: community requires a QQ group or join link", ErrInvalid)
	siteQQGroupPattern      = regexp.MustCompile(`^[1-9][0-9]{4,11}$`)
)

// SiteImageSlots lists every slot in display order.
func SiteImageSlots() []string {
	return []string{SiteImageCommunityQR, SiteImageRechargeWeChat, SiteImageRechargeAlipay}
}

func ValidSiteImageSlot(slot string) bool {
	switch slot {
	case SiteImageCommunityQR, SiteImageRechargeWeChat, SiteImageRechargeAlipay:
		return true
	}
	return false
}

type SiteImageInfo struct {
	Slot        string
	ContentType string
	SHA256      string
	Width       int
	Height      int
	UpdatedAt   time.Time
}

type SiteImage struct {
	SiteImageInfo
	Data []byte
}

type SiteSettingsFields struct {
	CommunityEnabled bool
	CommunityTitle   string
	CommunityQQGroup string
	CommunityJoinURL string
	CommunityNote    string
	RechargeEnabled  bool
	RechargeContact  string
	RechargeNote     string
}

type SiteSettings struct {
	SiteSettingsFields
	Revision  int64
	UpdatedAt time.Time
	Images    map[string]SiteImageInfo
}

type SiteActor struct {
	UserID    string
	SessionID string
	SourceIP  string
	RequestID string
}

type UpdateSiteSettingsParams struct {
	SiteSettingsFields
	ExpectedRevision int64
	Actor            SiteActor
}

// ValidateSiteSettings mirrors the table constraints and additionally rejects
// control characters, so pages can render every field as plain text.
func ValidateSiteSettings(f SiteSettingsFields) error {
	for _, item := range []struct {
		value     string
		limit     int
		multiline bool
	}{
		{f.CommunityTitle, 40, false},
		{f.CommunityNote, 500, true},
		{f.RechargeContact, 80, false},
		{f.RechargeNote, 1000, true},
	} {
		if !validSiteText(item.value, item.limit, item.multiline) {
			return fmt.Errorf("%w: invalid site text", ErrInvalid)
		}
	}
	if f.CommunityQQGroup != "" && !siteQQGroupPattern.MatchString(f.CommunityQQGroup) {
		return fmt.Errorf("%w: invalid QQ group", ErrInvalid)
	}
	if f.CommunityJoinURL != "" && !validSiteLink(f.CommunityJoinURL) {
		return fmt.Errorf("%w: invalid join link", ErrInvalid)
	}
	if f.CommunityEnabled && f.CommunityQQGroup == "" && f.CommunityJoinURL == "" {
		return ErrSiteCommunityContact
	}
	return nil
}

func validSiteText(value string, limit int, multiline bool) bool {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > limit || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if r == '\n' && multiline {
			continue
		}
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return false
		}
	}
	return true
}

// validSiteLink accepts only an absolute HTTPS URL without credentials. The
// dashboard opens it in a new tab with noopener and no referrer.
func validSiteLink(raw string) bool {
	if len(raw) > 500 || !strings.HasPrefix(raw, "https://") || strings.IndexFunc(raw, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 {
		return false
	}
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.Hostname() != "" && parsed.User == nil && parsed.Opaque == ""
}

func validSiteImage(image SiteImage) error {
	sum := sha256.Sum256(image.Data)
	if !ValidSiteImageSlot(image.Slot) || (image.ContentType != "image/png" && image.ContentType != "image/jpeg") ||
		len(image.Data) == 0 || len(image.Data) > SiteImageMaxBytes || image.SHA256 != hex.EncodeToString(sum[:]) ||
		image.Width < 1 || image.Width > SiteImageMaxDimension || image.Height < 1 || image.Height > SiteImageMaxDimension {
		return fmt.Errorf("%w: invalid site image", ErrInvalid)
	}
	return nil
}

const siteSettingsColumns = `community_enabled,community_title,community_qq_group,community_join_url,community_note,recharge_enabled,recharge_contact,recharge_note,revision,updated_at`

func readSiteSettings(ctx context.Context, tx *sql.Tx) (SiteSettings, error) {
	var settings SiteSettings
	f := &settings.SiteSettingsFields
	err := tx.QueryRowContext(ctx, `SELECT `+siteSettingsColumns+` FROM site_settings WHERE singleton`).Scan(
		&f.CommunityEnabled, &f.CommunityTitle, &f.CommunityQQGroup, &f.CommunityJoinURL, &f.CommunityNote,
		&f.RechargeEnabled, &f.RechargeContact, &f.RechargeNote, &settings.Revision, &settings.UpdatedAt)
	if err != nil {
		return SiteSettings{}, mapDBError("read site settings", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT slot,content_type,sha256,width,height,updated_at FROM site_images ORDER BY slot`)
	if err != nil {
		return SiteSettings{}, mapDBError("list site images", err)
	}
	defer rows.Close()
	settings.Images = make(map[string]SiteImageInfo)
	for rows.Next() {
		var info SiteImageInfo
		if err := rows.Scan(&info.Slot, &info.ContentType, &info.SHA256, &info.Width, &info.Height, &info.UpdatedAt); err != nil {
			return SiteSettings{}, mapDBError("scan site image", err)
		}
		settings.Images[info.Slot] = info
	}
	return settings, mapDBError("list site images", rows.Err())
}

// SiteSettings returns the text fields and image metadata from one snapshot.
func (s *Store) SiteSettings(ctx context.Context) (SiteSettings, error) {
	var settings SiteSettings
	err := s.withTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}, func(tx *sql.Tx) (err error) {
		settings, err = readSiteSettings(ctx, tx)
		return err
	})
	return settings, err
}

func (s *Store) SiteImage(ctx context.Context, slot string) (SiteImage, error) {
	if !ValidSiteImageSlot(slot) {
		return SiteImage{}, ErrNotFound
	}
	var image SiteImage
	err := s.db.QueryRowContext(ctx, `SELECT slot,content_type,sha256,width,height,updated_at,data FROM site_images WHERE slot=$1`, slot).Scan(
		&image.Slot, &image.ContentType, &image.SHA256, &image.Width, &image.Height, &image.UpdatedAt, &image.Data)
	return image, mapDBError("read site image", err)
}

// Every write advances the singleton revision first, so concurrent writes
// serialize on that row and readers see text and images change together.
func (s *Store) writeSiteSettings(ctx context.Context, actor SiteActor, event string, metadata map[string]any, write func(tx *sql.Tx, revision int64, now time.Time) error) (SiteSettings, error) {
	if actor.UserID == "" {
		return SiteSettings{}, ErrInvalid
	}
	var settings SiteSettings
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		now := s.now().UTC()
		var revision int64
		if err := tx.QueryRowContext(ctx, `UPDATE site_settings SET revision=revision+1,updated_at=$1 WHERE singleton RETURNING revision`, now).Scan(&revision); err != nil {
			return mapDBError("advance site settings revision", err)
		}
		if err := write(tx, revision, now); err != nil {
			return err
		}
		metadata["revision"] = revision
		data, err := marshalSafeMetadata(metadata)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO audit_events(occurred_at,actor_user_id,actor_session_id,event_type,severity,success,source_ip,subject_type,subject_id,request_id,metadata) VALUES($1,$2::uuid,$3::uuid,$4,'info',true,$5::inet,'site_settings','site',$6,$7::jsonb)`,
			now, actor.UserID, valueOrNil(actor.SessionID), event, valueOrNil(actor.SourceIP), valueOrNil(actor.RequestID), data); err != nil {
			return mapDBError("audit site settings", err)
		}
		settings, err = readSiteSettings(ctx, tx)
		return err
	})
	return settings, err
}

func (s *Store) UpdateSiteSettings(ctx context.Context, params UpdateSiteSettingsParams) (SiteSettings, error) {
	if err := ValidateSiteSettings(params.SiteSettingsFields); err != nil {
		return SiteSettings{}, err
	}
	if params.ExpectedRevision < 1 {
		return SiteSettings{}, ErrInvalid
	}
	f := params.SiteSettingsFields
	metadata := map[string]any{"community_enabled": f.CommunityEnabled, "recharge_enabled": f.RechargeEnabled}
	return s.writeSiteSettings(ctx, params.Actor, "site.settings_updated", metadata, func(tx *sql.Tx, revision int64, _ time.Time) error {
		// The revision was just advanced under the row lock.
		if revision != params.ExpectedRevision+1 {
			return ErrSiteSettingsConflict
		}
		_, err := tx.ExecContext(ctx, `UPDATE site_settings SET community_enabled=$1,community_title=$2,community_qq_group=$3,community_join_url=$4,community_note=$5,recharge_enabled=$6,recharge_contact=$7,recharge_note=$8 WHERE singleton`,
			f.CommunityEnabled, f.CommunityTitle, f.CommunityQQGroup, f.CommunityJoinURL, f.CommunityNote, f.RechargeEnabled, f.RechargeContact, f.RechargeNote)
		return mapDBError("update site settings", err)
	})
}

func (s *Store) PutSiteImage(ctx context.Context, image SiteImage, actor SiteActor) (SiteSettings, error) {
	if err := validSiteImage(image); err != nil {
		return SiteSettings{}, err
	}
	metadata := map[string]any{"slot": image.Slot, "sha256": image.SHA256, "bytes": len(image.Data)}
	return s.writeSiteSettings(ctx, actor, "site.image_updated", metadata, func(tx *sql.Tx, _ int64, now time.Time) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO site_images(slot,content_type,data,sha256,width,height,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT(slot) DO UPDATE SET content_type=EXCLUDED.content_type,data=EXCLUDED.data,sha256=EXCLUDED.sha256,width=EXCLUDED.width,height=EXCLUDED.height,updated_at=EXCLUDED.updated_at`,
			image.Slot, image.ContentType, image.Data, image.SHA256, image.Width, image.Height, now)
		return mapDBError("save site image", err)
	})
}

func (s *Store) DeleteSiteImage(ctx context.Context, slot string, actor SiteActor) (SiteSettings, error) {
	if !ValidSiteImageSlot(slot) {
		return SiteSettings{}, ErrInvalid
	}
	return s.writeSiteSettings(ctx, actor, "site.image_deleted", map[string]any{"slot": slot}, func(tx *sql.Tx, _ int64, _ time.Time) error {
		result, err := tx.ExecContext(ctx, `DELETE FROM site_images WHERE slot=$1`, slot)
		if err != nil {
			return mapDBError("delete site image", err)
		}
		if count, _ := result.RowsAffected(); count == 0 {
			return ErrNotFound
		}
		return nil
	})
}
