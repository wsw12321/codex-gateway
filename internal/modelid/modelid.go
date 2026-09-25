// Package modelid compares three independent number-choice answers with a
// pinned WhatsMyLLM fingerprint bank. Its result is a statistical match, not
// proof of a model's identity.
package modelid

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
)

const (
	BankVersion      = "2026.09.5"
	ReferenceVersion = BankVersion
	valueMin         = 1
	valueMax         = 355
	dimension        = valueMax - valueMin + 1
	alpha            = 0.5
	inputLimit       = 20000
)

//go:embed data/*.json
var files embed.FS

var expectedHashes = map[string]string{
	"data/bank.json":       "d21c1413eeac2f5238f37bc97df0299591433c14a1bbd5534546e95697fbd11f",
	"data/challenges.json": "dd3772110658f47ce066c4770feea28254c88aad1047791da2b7a27b9a9b671d",
	"data/gates.json":      "4a3ad9dea0a63edbf7735e0cced5643662c6ceadeb42db8f52c65b12a6b5705c",
	"data/verdict.json":    "26ee5b482b73fc93e75b3b43aa553ffca3992ffbcf7885e97f5b98be71eaca52",
}

type Challenge struct {
	ID              string `json:"challenge_id"`
	Prompt          string `json:"prompt"`
	RequestedCount  int    `json:"requested_count"`
	StrictThreshold int    `json:"strict_threshold"`
}

type challengeFile struct {
	Sets []struct {
		Environment string      `json:"environment"`
		Language    string      `json:"language"`
		Style       string      `json:"style"`
		Challenges  []Challenge `json:"challenges"`
	} `json:"sets"`
}

type rule struct {
	ID          string          `json:"id"`
	Level       string          `json:"level"`
	Stat        string          `json:"stat"`
	Op          string          `json:"op"`
	Threshold   json.RawMessage `json:"threshold"`
	AppliesFrom int             `json:"applies_from"`
}

type gateSpec struct {
	Version  string `json:"version"`
	ValueMin int    `json:"value_min"`
	ValueMax int    `json:"value_max"`
	Rules    []rule `json:"rules"`
}

type verdictSpec struct {
	Version    string `json:"version"`
	Thresholds struct {
		FitMin    float64 `json:"fit_min"`
		MarginMin float64 `json:"margin_min"`
		FamilyMin float64 `json:"family_min"`
	} `json:"thresholds"`
}

type model struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name"`
	Family      string    `json:"family"`
	FamilyName  string    `json:"family_name"`
	Counts      []float64 `json:"counts"`
}

type artifact struct {
	Weight               float64       `json:"weight"`
	FeatureMean          []float64     `json:"feature_mean"`
	FeatureScale         []float64     `json:"feature_scale"`
	NuisanceBasis        [][]float64   `json:"nuisance_basis"`
	Centroids            [][]float64   `json:"centroids"`
	EnvironmentCentroids [][][]float64 `json:"environment_centroids"`
}

type bankFile struct {
	Schema string  `json:"schema"`
	Models []model `json:"models"`
	Robust struct {
		ModelOrder   []string `json:"model_order"`
		Hellinger    artifact `json:"hellinger"`
		OrderedBlock artifact `json:"ordered_blocks"`
	} `json:"robust"`
	Calibration map[string]struct {
		Beta float64 `json:"beta"`
	} `json:"calibration"`
}

var (
	challenges          [3]Challenge
	gates               gateSpec
	verdict             verdictSpec
	bank                bankFile
	digits              = regexp.MustCompile(`[0-9]+`)
	fencePattern        = regexp.MustCompile("(?is)^```(?:json|text|plaintext)?[ \\t]*\\r?\\n([\\s\\S]*?)\\r?\\n```[ \\t]*$")
	arrayPattern        = regexp.MustCompile(`^\[\s*(?:\d+\s*(?:,\s*\d+\s*)*)?\]$`)
	numberArrayPattern  = regexp.MustCompile(`\[[0-9,\s]*\]`)
	digitNewlinePattern = regexp.MustCompile(`[0-9][^\S\r\n\x{2028}\x{2029}]*[\r\n\x{2028}\x{2029}]\s*[0-9]`)
)

// ReplyError reports why one of the three answers cannot be scored. Index is
// zero based. The answer itself is intentionally never retained in this error.
type ReplyError struct {
	Index int
	Rule  string
}

func (e *ReplyError) Error() string {
	return fmt.Sprintf("model identification reply %d: %s", e.Index+1, e.Rule)
}

var ErrInvalidReply = errors.New("model identification reply invalid")

func (e *ReplyError) Is(target error) bool { return target == ErrInvalidReply }

// Result contains only the score summary. A weak match leaves MatchedModel
// empty even though ClosestModel names the highest ranked bank candidate.
type Result struct {
	ClosestModel string
	ClosestName  string
	MatchedModel string
	Family       string
	Level        string // match, family_only, insufficient
	Fit          float64
	Margin       float64
	BankVersion  string
	Candidates   []Candidate
}

type Candidate struct {
	Model       string
	DisplayName string
	Family      string
	Probability float64
	Fit         float64
}

func init() {
	for name, want := range expectedHashes {
		data, err := files.ReadFile(name)
		if err != nil {
			panic(err)
		}
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != want {
			panic("modelid: bundled WhatsMyLLM data hash mismatch: " + name)
		}
		var target any
		switch name {
		case "data/challenges.json":
			target = new(challengeFile)
		case "data/gates.json":
			target = &gates
		case "data/verdict.json":
			target = &verdict
		case "data/bank.json":
			target = &bank
		}
		if err := json.Unmarshal(data, target); err != nil {
			panic(fmt.Errorf("modelid: parse %s: %w", name, err))
		}
		if cf, ok := target.(*challengeFile); ok {
			for _, set := range cf.Sets {
				if set.Environment == "environment-05" && set.Language == "en" && set.Style == "prose" {
					if len(set.Challenges) != 3 {
						panic("modelid: expected three environment-05 challenges")
					}
					copy(challenges[:], set.Challenges)
				}
			}
		}
	}
	if bank.Schema != "robust-number-fingerprint-bank" || len(bank.Models) != 21 || len(bank.Robust.ModelOrder) != len(bank.Models) ||
		gates.Version != "2026.09.1" || verdict.Version != "2026.09.1" || gates.ValueMin != valueMin || gates.ValueMax != valueMax ||
		challenges[0].ID != "query-13" || challenges[1].ID != "query-14" || challenges[2].ID != "query-15" {
		panic("modelid: unexpected bundled dataset")
	}
	for i, m := range bank.Models {
		if m.ID != bank.Robust.ModelOrder[i] || len(m.Counts) != dimension {
			panic("modelid: invalid model bank")
		}
	}
	checkArtifact(bank.Robust.Hellinger, dimension, len(bank.Models), 0)
	checkArtifact(bank.Robust.OrderedBlock, 74, len(bank.Models), 12)
}

func checkArtifact(a artifact, dim, models, environments int) {
	if len(a.FeatureMean) != dim || len(a.FeatureScale) != dim || len(a.Centroids) != models || len(a.EnvironmentCentroids) != environments {
		panic("modelid: invalid bank artifact dimensions")
	}
	for i, scale := range a.FeatureScale {
		if scale <= 0 || math.IsNaN(scale) || math.IsInf(scale, 0) || math.IsNaN(a.FeatureMean[i]) {
			panic("modelid: invalid bank feature scale")
		}
	}
	for _, v := range a.NuisanceBasis {
		if len(v) != dim {
			panic("modelid: invalid nuisance basis")
		}
	}
	for _, v := range a.Centroids {
		if len(v) != dim {
			panic("modelid: invalid centroid")
		}
	}
	for _, env := range a.EnvironmentCentroids {
		if len(env) != models {
			panic("modelid: invalid environment centroids")
		}
		for _, v := range env {
			if len(v) != dim {
				panic("modelid: invalid environment centroid")
			}
		}
	}
}

// Challenges returns the fixed English, plain-text WhatsMyLLM prompts. Each
// prompt must be sent in a fresh conversation with no tools or added instruction.
func Challenges() []Challenge { return append([]Challenge(nil), challenges[:]...) }

// Score follows the pinned WhatsMyLLM core, input gates, and open-set verdict.
// All three answers must pass; no answer is retained after this call.
func Score(replies []string) (Result, error) {
	if len(replies) != len(challenges) {
		return Result{}, fmt.Errorf("modelid: need exactly %d replies", len(challenges))
	}
	var parsed [3][]int
	seen := make(map[string]bool, 3)
	for i, reply := range replies {
		if len(utf16.Encode([]rune(reply))) > inputLimit {
			return Result{}, &ReplyError{Index: i, Rule: "limit"}
		}
		text, transportRule := plainReply(reply)
		if transportRule != "" {
			return Result{}, &ReplyError{Index: i, Rule: transportRule}
		}
		parsed[i] = parseNumbers(text)
		if ruleID := gate(parsed[i], challenges[i].RequestedCount); ruleID != "" {
			return Result{}, &ReplyError{Index: i, Rule: ruleID}
		}
		key := numberKey(parsed[i])
		if seen[key] {
			return Result{}, &ReplyError{Index: i, Rule: "duplicate"}
		}
		seen[key] = true
	}
	return scoreParsed(parsed), nil
}

func parseNumbers(text string) []int {
	positions := digits.FindAllStringIndex(text, -1)
	var runs [][]int
	current := make([]int, 0, 350)
	previousEnd := 0
	for _, p := range positions {
		if len(current) > 0 && strings.IndexFunc(text[previousEnd:p[0]], unicode.IsLetter) >= 0 {
			runs = append(runs, current)
			current = make([]int, 0, 350)
		}
		if n, err := strconv.Atoi(text[p[0]:p[1]]); err == nil && n >= valueMin && n <= valueMax {
			current = append(current, n)
		}
		previousEnd = p[1]
	}
	if len(current) > 0 {
		runs = append(runs, current)
	}
	var longest []int
	for _, run := range runs {
		if len(run) > len(longest) {
			longest = run
		}
	}
	return longest
}

func numberKey(values []int) string {
	var b strings.Builder
	for _, n := range values {
		b.WriteString(strconv.Itoa(n))
		b.WriteByte(',')
	}
	return b.String()
}

func gate(values []int, requested int) string {
	length := len(values)
	unique := make(map[int]struct{}, length)
	inc, dec := 0, 0
	steps := map[int]int{}
	modal := 0
	for i, n := range values {
		unique[n] = struct{}{}
		if i == 0 {
			continue
		}
		delta := n - values[i-1]
		if delta >= 0 {
			inc++
		}
		if delta <= 0 {
			dec++
		}
		if delta < 0 {
			delta = -delta
		}
		steps[delta]++
		if steps[delta] > modal {
			modal = steps[delta]
		}
	}
	denominator := max(length-1, 1)
	stats := map[string]float64{
		"length": float64(length), "unique_count": float64(len(unique)),
		"unique_ratio":        float64(len(unique)) / float64(max(1, min(length, dimension))),
		"monotone_fraction":   float64(max(inc, dec)) / float64(denominator),
		"arithmetic_fraction": float64(modal) / float64(denominator),
	}
	for _, r := range gates.Rules {
		if length < r.AppliesFrom {
			continue
		}
		threshold := thresholdFor(r.Threshold, requested)
		value, ok := stats[r.Stat]
		if !ok {
			panic("modelid: unknown gate stat: " + r.Stat)
		}
		fired := false
		switch r.Op {
		case "lt":
			fired = value < threshold
		case "lte":
			fired = value <= threshold
		case "gt":
			fired = value > threshold
		case "gte":
			fired = value >= threshold
		default:
			panic("modelid: unknown gate op: " + r.Op)
		}
		if fired {
			return r.ID
		}
	}
	return ""
}

func thresholdFor(raw json.RawMessage, requested int) float64 {
	var number float64
	if err := json.Unmarshal(raw, &number); err == nil {
		return number
	}
	var object struct {
		Absolute float64 `json:"absolute"`
		Fraction float64 `json:"fraction_of_requested"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		panic("modelid: invalid gate threshold")
	}
	if requested == 0 {
		return object.Absolute
	}
	return math.Max(object.Absolute, math.Ceil(object.Fraction*float64(requested)))
}

func plainReply(text string) (string, string) {
	var inputRule string
	text, inputRule = unfence(text)
	if inputRule != "" {
		return "", inputRule
	}
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") {
		var envelope map[string]any
		if json.Unmarshal([]byte(trimmed), &envelope) != nil || envelope == nil {
			return "", "api_invalid"
		}
		if choices, ok := envelope["choices"].([]any); ok {
			if len(choices) != 1 {
				return "", "api_multiple"
			}
			choice, _ := choices[0].(map[string]any)
			message, _ := choice["message"].(map[string]any)
			content, _ := message["content"].(string)
			toolCalls, _ := message["tool_calls"].([]any)
			if envelope["object"] != "chat.completion" || choice["finish_reason"] != "stop" || message["role"] != "assistant" ||
				len(toolCalls) > 0 || jsTruthy(message["function_call"]) || jsTruthy(message["refusal"]) || strings.TrimSpace(content) == "" {
				return "", "api_tools"
			}
			return numberReply(content)
		}
		if envelope["type"] == "message" && envelope["role"] == "assistant" {
			items, ok := envelope["content"].([]any)
			if !ok || envelope["stop_reason"] != "end_turn" || len(items) != 1 {
				return "", "api_tools"
			}
			item, _ := items[0].(map[string]any)
			content, _ := item["text"].(string)
			if item["type"] != "text" || strings.TrimSpace(content) == "" {
				return "", "api_tools"
			}
			return numberReply(content)
		}
		return "", "api_invalid"
	}
	return numberReply(text)
}

func jsTruthy(value any) bool {
	switch x := value.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	default:
		return true
	}
}

func unfence(text string) (string, string) {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "```") {
		// The official workbench accepts only a single complete code fence.
		parts := fencePattern.FindStringSubmatch(trimmed)
		if parts == nil || strings.Contains(parts[1], "```") {
			return "", "api_invalid"
		}
		return parts[1], ""
	}
	return text, ""
}

func numberReply(text string) (string, string) {
	var inputRule string
	text, inputRule = unfence(text)
	if inputRule != "" {
		return "", inputRule
	}
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(strings.ToLower(trimmed), "data:") || strings.HasPrefix(strings.ToLower(trimmed), "event:") {
		return "", "api_stream"
	}
	if splitNumberArray(text) {
		return "", "wrapped_number"
	}
	if strings.HasPrefix(trimmed, "{") {
		return "", "api_invalid"
	}
	if strings.HasPrefix(trimmed, "[") {
		var array []any
		if err := json.Unmarshal([]byte(trimmed), &array); err != nil {
			return "", "api_invalid"
		}
		for _, item := range array {
			switch item.(type) {
			case []any, map[string]any:
				return "", "api_multiple"
			}
		}
		if !arrayPattern.MatchString(trimmed) {
			return "", "api_array"
		}
	}
	return text, ""
}

func splitNumberArray(text string) bool {
	for _, candidate := range numberArrayPattern.FindAllString(text, -1) {
		if !strings.Contains(candidate, ",") || !digitNewlinePattern.MatchString(candidate) {
			continue
		}
		var original []int
		if json.Unmarshal([]byte(candidate), &original) == nil {
			continue
		}
		probe := strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return -1
			}
			return r
		}, candidate)
		var values []int
		if json.Unmarshal([]byte(probe), &values) != nil {
			continue
		}
		allInRange := true
		for _, value := range values {
			if value < valueMin || value > valueMax {
				allInRange = false
				break
			}
		}
		if allInRange {
			return true
		}
	}
	return false
}

func scoreParsed(parsed [3][]int) Result {
	combined := make([]float64, len(bank.Models))
	fits := make([]float64, len(bank.Models))
	for _, values := range parsed {
		counts := countNumbers(values)
		marginal, raw := marginalScores(counts)
		ordered := orderedScores(values)
		for j := range combined {
			combined[j] += ((1-bank.Robust.OrderedBlock.Weight)*marginal[j] + bank.Robust.OrderedBlock.Weight*ordered[j]) / float64(len(parsed))
			fits[j] += raw[j] / float64(len(parsed))
		}
	}
	beta := bank.Calibration["3"].Beta
	prob := softmax(combined, beta)
	results := make([]Candidate, len(bank.Models))
	for i, m := range bank.Models {
		results[i] = Candidate{Model: m.ID, DisplayName: m.DisplayName, Family: m.Family, Probability: prob[i], Fit: fits[i]}
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].Probability > results[j].Probability })
	// The pinned bank currently has one entry per bare model. Keep explicit
	// grouping to match verdict.js when a future bank adds channel entries.
	grouped := make([]Candidate, 0, len(results))
	groups := make(map[string]int, len(results))
	for _, candidate := range results {
		bare := strings.SplitN(candidate.Model, "@", 2)[0]
		if j, ok := groups[bare]; ok {
			grouped[j].Probability += candidate.Probability
			if candidate.Fit > grouped[j].Fit {
				grouped[j].Fit = candidate.Fit
			}
		} else {
			candidate.Model = bare
			groups[bare] = len(grouped)
			grouped = append(grouped, candidate)
		}
	}
	sort.SliceStable(grouped, func(i, j int) bool { return grouped[i].Probability > grouped[j].Probability })
	top := grouped[0]
	otherFit := 0.0
	if len(grouped) > 1 {
		otherFit = grouped[1].Fit
		for _, candidate := range grouped[2:] {
			if candidate.Fit > otherFit {
				otherFit = candidate.Fit
			}
		}
	}
	margin := top.Fit - otherFit
	familyProbability := 0.0
	for _, candidate := range grouped {
		if candidate.Family == top.Family {
			familyProbability += candidate.Probability
		}
	}
	result := Result{ClosestModel: top.Model, ClosestName: top.DisplayName, Fit: top.Fit, Margin: margin, BankVersion: BankVersion, Candidates: grouped}
	switch {
	case top.Fit < verdict.Thresholds.FitMin:
		result.Level = "insufficient"
	case margin >= verdict.Thresholds.MarginMin:
		result.Level, result.MatchedModel, result.Family = "match", top.Model, top.Family
	case familyProbability >= verdict.Thresholds.FamilyMin:
		result.Level, result.Family = "family_only", top.Family
	default:
		result.Level = "insufficient"
	}
	return result
}

func countNumbers(values []int) []float64 {
	counts := make([]float64, dimension)
	for _, n := range values {
		counts[n-valueMin]++
	}
	return counts
}

func marginalScores(counts []float64) ([]float64, []float64) {
	a := bank.Robust.Hellinger
	total := alpha * dimension
	for _, n := range counts {
		total += n
	}
	projected := make([]float64, dimension)
	for i, n := range counts {
		projected[i] = (math.Sqrt((n+alpha)/total) - a.FeatureMean[i]) / a.FeatureScale[i]
	}
	projected = normalize(subtractBasis(projected, a.NuisanceBasis))
	raw := make([]float64, len(a.Centroids))
	for i, centroid := range a.Centroids {
		raw[i] = dot(projected, centroid)
	}
	return standardize(raw), raw
}

func orderedScores(values []int) []float64 {
	a := bank.Robust.OrderedBlock
	feature := orderedFeature(values)
	standard := make([]float64, len(feature))
	for i, n := range feature {
		standard[i] = (n - a.FeatureMean[i]) / a.FeatureScale[i]
	}
	unit := normalize(standard)
	environmentScores := make([][]float64, len(a.EnvironmentCentroids))
	for e, env := range a.EnvironmentCentroids {
		environmentScores[e] = make([]float64, len(env))
		for i, centroid := range env {
			environmentScores[e][i] = dot(unit, centroid)
		}
	}
	template := make([]float64, len(a.Centroids))
	for i := range template {
		template[i] = environmentScores[0][i]
		for _, env := range environmentScores[1:] {
			if env[i] > template[i] {
				template[i] = env[i]
			}
		}
	}
	template = standardize(template)
	projected := normalize(subtractBasis(standard, a.NuisanceBasis))
	nuisance := make([]float64, len(a.Centroids))
	for i, centroid := range a.Centroids {
		nuisance[i] = dot(projected, centroid)
	}
	nuisance = standardize(nuisance)
	for i := range template {
		template[i] = 0.5*template[i] + 0.5*nuisance[i]
	}
	return standardize(template)
}

func orderedFeature(values []int) []float64 {
	feature := make([]float64, 0, 74)
	base, remainder := len(values)/4, len(values)%4
	start := 0
	for chunk := 0; chunk < 4; chunk++ {
		size := base
		if chunk < remainder {
			size++
		}
		bins := make([]float64, 16)
		for i := range bins {
			bins[i] = 0.5
		}
		for _, n := range values[start : start+size] {
			bins[min(15, int(math.Floor(float64(n-1)/355*16)))]++
		}
		total := 0.0
		for _, n := range bins {
			total += n
		}
		for _, n := range bins {
			feature = append(feature, math.Sqrt(n/total))
		}
		start += size
	}
	digits := make([]float64, 10)
	for i := range digits {
		digits[i] = 0.5
	}
	for _, n := range values {
		digits[n%10]++
	}
	total := 0.0
	for _, n := range digits {
		total += n
	}
	for _, n := range digits {
		feature = append(feature, math.Sqrt(n/total))
	}
	return feature
}

func dot(left, right []float64) float64 {
	total := 0.0
	for i, n := range left {
		total += n * right[i]
	}
	return total
}

func normalize(values []float64) []float64 {
	scale := math.Max(math.Sqrt(dot(values, values)), 1e-12)
	result := make([]float64, len(values))
	for i, n := range values {
		result[i] = n / scale
	}
	return result
}

func subtractBasis(values []float64, basis [][]float64) []float64 {
	result := append([]float64(nil), values...)
	for _, vector := range basis {
		projection := dot(result, vector)
		for i := range result {
			result[i] -= projection * vector[i]
		}
	}
	return result
}

func standardize(values []float64) []float64 {
	mean := 0.0
	for _, n := range values {
		mean += n
	}
	mean /= float64(len(values))
	variance := 0.0
	for _, n := range values {
		variance += (n - mean) * (n - mean)
	}
	variance /= float64(len(values))
	scale := math.Max(math.Sqrt(variance), 1e-12)
	result := make([]float64, len(values))
	for i, n := range values {
		result[i] = (n - mean) / scale
	}
	return result
}

func softmax(values []float64, beta float64) []float64 {
	maxScore := values[0] * beta
	for _, n := range values[1:] {
		if n*beta > maxScore {
			maxScore = n * beta
		}
	}
	result := make([]float64, len(values))
	total := 0.0
	for i, n := range values {
		result[i] = math.Exp(beta*n - maxScore)
		total += result[i]
	}
	for i := range result {
		result[i] /= total
	}
	return result
}
