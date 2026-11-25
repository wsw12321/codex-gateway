package server

import (
	"strings"
	"testing"
)

const (
	testBrandName    = "百川阁"
	testBrandSlogan  = "众源成流，聚流成渊"
	testSellerSlogan = "你出水源，我来汇流"
	testBuyerSlogan  = "一处接入，百家模型"
)

// dashboardSection returns the markup of one dashboard view, from its
// data-section attribute up to the next view.
func dashboardSection(t *testing.T, html, section string) string {
	t.Helper()
	marker := `data-section="` + section + `"`
	start := strings.Index(html, marker)
	if start < 0 {
		t.Fatalf("dashboard HTML has no %s section", section)
	}
	rest := html[start+len(marker):]
	if end := strings.Index(rest, `data-section="`); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

func TestBrandNameAndMainSloganAreConsistent(t *testing.T) {
	t.Parallel()

	html := string(indexHTML)
	for _, required := range []string{
		`<title>` + testBrandName + ` · ` + testBrandSlogan + `</title>`,
		`<meta name="description" content="` + testBrandName + `：` + testBrandSlogan + `。`,
		`aria-label="` + testBrandName + `首页"`,
		`<span><strong>` + testBrandName + `</strong><small>BAICHUAN GE</small></span>`,
		`<p class="eyebrow">BAICHUAN GE · AI GATEWAY</p>`,
		`<h2>众源成流，<br><span>聚流成渊。</span></h2>`,
		`<h1>欢迎回到` + testBrandName + `</h1>`,
		`<h1 id="join-title">加入` + testBrandName + `</h1>`,
		`<footer class="auth-footer"><span>` + testBrandName + ` · ` + testBrandSlogan + `</span>`,
		`aria-label="` + testBrandName + `控制台"`,
		`<div><strong>` + testBrandName + `</strong><small>` + testBrandSlogan + `</small></div>`,
		`<p class="workspace-eyebrow">` + testBrandName + ` / 控制台</p>`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("dashboard HTML is missing brand markup %s", required)
		}
	}
	javascript := string(appJS)
	for _, required := range []string{
		`const brandName = "` + testBrandName + `";`,
		"document.title = `${pageTitle} · ${brandName}`;",
		"byId(\"join-title\").textContent = `加入${brandName}`;",
	} {
		if !strings.Contains(javascript, required) {
			t.Fatalf("dashboard JavaScript is missing brand code %s", required)
		}
	}
	callback := string(oidcCallbackHTML)
	for _, required := range []string{
		`<title>账号授权 · ` + testBrandName + `</title>`,
		`<span><strong>` + testBrandName + `</strong><small>BAICHUAN GE</small></span>`,
		`<p class="eyebrow">BAICHUAN GE</p>`,
		`<footer class="auth-footer">` + testBrandName + ` · ` + testBrandSlogan + `</footer>`,
	} {
		if !strings.Contains(callback, required) {
			t.Fatalf("OIDC callback is missing brand markup %s", required)
		}
	}
	cpaIndex, err := cpaAssets.ReadFile("assets/cpa/index.html")
	if err != nil {
		t.Fatal(err)
	}
	cpaPanel, err := cpaAssets.ReadFile("assets/cpa/panel.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`<title>CPA 账号管理 · ` + testBrandName + `</title>`,
		`<span><strong>` + testBrandName + `</strong><small>BAICHUAN GE</small></span>`,
	} {
		if !strings.Contains(string(cpaIndex), required) {
			t.Fatalf("CPA panel page is missing brand markup %s", required)
		}
	}
	for _, required := range []string{testBrandName + ` · 站点管理`, testBrandName + ` · 账号管理`} {
		if !strings.Contains(string(cpaPanel), required) {
			t.Fatalf("CPA panel bundle is missing %s", required)
		}
	}

	// The previous brand and the relay-station wording are gone from every
	// user-facing page.
	for name, content := range map[string]string{
		"dashboard HTML": html, "dashboard JavaScript": javascript, "OIDC callback": callback,
		"OIDC callback JavaScript": string(oidcCallbackJS), "CPA panel page": string(cpaIndex), "CPA panel bundle": string(cpaPanel),
	} {
		for _, retired := range []string{"水源喵", "SHUIYUAN", "中转站", "AI RELAY"} {
			if strings.Contains(content, retired) {
				t.Fatalf("%s still contains retired brand wording %q", name, retired)
			}
		}
	}
}

func TestSellerAndBuyerPagesCarryTheirSubSlogans(t *testing.T) {
	t.Parallel()

	html := string(indexHTML)
	seller := `<p class="page-slogan" data-page-role="seller">` + testSellerSlogan + `</p>`
	buyer := `<p class="page-slogan" data-page-role="buyer">` + testBuyerSlogan + `</p>`
	for section, want := range map[string]string{
		"providers": seller,
		"my-plans":  seller,
		// Owners share this view for the platform egress; only members see the seller slogan.
		"egress-servers": `<p class="page-slogan member-market hidden" data-page-role="seller">` + testSellerSlogan + `</p>`,
		"plans":          buyer,
	} {
		markup := dashboardSection(t, html, section)
		if !strings.Contains(markup, want) {
			t.Fatalf("%s section is missing sub-slogan %s", section, want)
		}
		if strings.Count(markup, `class="page-slogan`) != 1 {
			t.Fatalf("%s section must carry exactly one sub-slogan", section)
		}
	}
	if got := strings.Count(html, testSellerSlogan); got != 3 {
		t.Fatalf("seller sub-slogan appears %d times, want the three seller pages", got)
	}
	if got := strings.Count(html, testBuyerSlogan); got != 1 {
		t.Fatalf("buyer sub-slogan appears %d times, want only the plans page", got)
	}
	if !strings.Contains(string(styleCSS), `.view > .section-head .page-slogan {`) {
		t.Fatal("sub-slogans need their own visible style; section-head eyebrows are hidden")
	}
}
