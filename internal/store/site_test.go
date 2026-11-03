package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestValidateSiteSettings(t *testing.T) {
	t.Parallel()
	valid := SiteSettingsFields{CommunityEnabled: true, CommunityTitle: "水源喵交流群", CommunityQQGroup: "123456789",
		CommunityJoinURL: "https://qm.qq.com/q/AbC?x=1", CommunityNote: "第一行\n第二行", RechargeEnabled: true, RechargeContact: "群主", RechargeNote: strings.Repeat("字", 1000)}
	if err := ValidateSiteSettings(valid); err != nil {
		t.Fatalf("valid settings: %v", err)
	}
	if err := ValidateSiteSettings(SiteSettingsFields{}); err != nil {
		t.Fatalf("empty disabled settings: %v", err)
	}
	for _, tt := range []struct {
		name   string
		mutate func(*SiteSettingsFields)
	}{
		{"long title", func(f *SiteSettingsFields) { f.CommunityTitle = strings.Repeat("字", 41) }},
		{"multiline title", func(f *SiteSettingsFields) { f.CommunityTitle = "a\nb" }},
		{"padded contact", func(f *SiteSettingsFields) { f.RechargeContact = " 群主" }},
		{"control in note", func(f *SiteSettingsFields) { f.CommunityNote = "a\x1bb" }},
		{"line separator", func(f *SiteSettingsFields) { f.RechargeNote = "a b" }},
		{"long recharge note", func(f *SiteSettingsFields) { f.RechargeNote = strings.Repeat("字", 1001) }},
		{"short QQ group", func(f *SiteSettingsFields) { f.CommunityQQGroup = "1234" }},
		{"leading zero QQ group", func(f *SiteSettingsFields) { f.CommunityQQGroup = "012345" }},
		{"spaced QQ group", func(f *SiteSettingsFields) { f.CommunityQQGroup = "123 456" }},
		{"plain HTTP link", func(f *SiteSettingsFields) { f.CommunityJoinURL = "http://qm.qq.com/q/abc" }},
		{"script link", func(f *SiteSettingsFields) { f.CommunityJoinURL = "javascript:alert(1)" }},
		{"uppercase scheme", func(f *SiteSettingsFields) { f.CommunityJoinURL = "HTTPS://qm.qq.com" }},
		{"credential link", func(f *SiteSettingsFields) { f.CommunityJoinURL = "https://user:pass@qm.qq.com" }},
		{"hostless link", func(f *SiteSettingsFields) { f.CommunityJoinURL = "https:///path" }},
		{"spaced link", func(f *SiteSettingsFields) { f.CommunityJoinURL = "https://qm.qq.com/a b" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := valid
			tt.mutate(&f)
			if err := ValidateSiteSettings(f); !errors.Is(err, ErrInvalid) {
				t.Fatalf("error = %v, want ErrInvalid", err)
			}
		})
	}
	enabledWithoutContact := valid
	enabledWithoutContact.CommunityQQGroup, enabledWithoutContact.CommunityJoinURL = "", ""
	if err := ValidateSiteSettings(enabledWithoutContact); !errors.Is(err, ErrSiteCommunityContact) {
		t.Fatalf("enabled community without contact: %v", err)
	}
	enabledWithoutContact.CommunityEnabled = false
	if err := ValidateSiteSettings(enabledWithoutContact); err != nil {
		t.Fatalf("disabled community without contact: %v", err)
	}
}

func TestValidSiteImageRequiresMatchingDigestAndBounds(t *testing.T) {
	t.Parallel()
	data := []byte("normalized")
	sum := sha256.Sum256(data)
	valid := SiteImage{SiteImageInfo: SiteImageInfo{Slot: SiteImageRechargeAlipay, ContentType: "image/png", SHA256: hex.EncodeToString(sum[:]), Width: 1, Height: SiteImageMaxDimension}, Data: data}
	if err := validSiteImage(valid); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*SiteImage){
		"unknown slot":   func(i *SiteImage) { i.Slot = "banner" },
		"svg":            func(i *SiteImage) { i.ContentType = "image/svg+xml" },
		"digest":         func(i *SiteImage) { i.Data = []byte("changed") },
		"empty":          func(i *SiteImage) { i.Data = nil },
		"too tall":       func(i *SiteImage) { i.Height = SiteImageMaxDimension + 1 },
		"zero width":     func(i *SiteImage) { i.Width = 0 },
		"oversized data": func(i *SiteImage) { i.Data = make([]byte, SiteImageMaxBytes+1) },
	} {
		image := valid
		mutate(&image)
		if err := validSiteImage(image); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: error = %v", name, err)
		}
	}
}
