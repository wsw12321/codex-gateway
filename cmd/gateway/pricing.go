package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/wsw/codex-gateway/internal/config"
)

const pricingUsage = "usage: gateway pricing show [-builtin] [-model ID] | check | migrate-env < legacy-pricing.json"

// runPricing implements the offline pricing tools. It never touches the
// database and only reads the built-in catalog, the override file named by
// GATEWAY_USAGE_PRICING_OVERRIDE_FILE and, for migrate-env, stdin. Operators
// run it through scripts/pricing.sh, in a container without network or
// secrets.
func runPricing(arguments []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) error {
	if len(arguments) == 0 {
		return errors.New(pricingUsage)
	}
	switch arguments[0] {
	case "show":
		return pricingShow(arguments[1:], stdout, stderr, getenv)
	case "check":
		if len(arguments) != 1 {
			return errors.New(pricingUsage)
		}
		return pricingCheck(stdout, getenv)
	case "migrate-env":
		if len(arguments) != 1 {
			return errors.New(pricingUsage)
		}
		return pricingMigrateEnv(stdin, stdout, stderr)
	default:
		return fmt.Errorf("unknown pricing command %q; %s", arguments[0], pricingUsage)
	}
}

// pricingShow prints the effective catalog, or with -builtin the catalog
// compiled into this binary. With -model it prints an override document
// holding only that model's rule: a starting point for an override file that
// never depends on the current file being valid when -builtin is set.
func pricingShow(arguments []string, stdout, stderr io.Writer, getenv func(string) string) error {
	flags := flag.NewFlagSet("gateway pricing show", flag.ContinueOnError)
	flags.SetOutput(stderr)
	builtIn := flags.Bool("builtin", false, "ignore the override file and show the built-in catalog")
	model := flags.String("model", "", "print an override document for this model only")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New(pricingUsage)
	}
	var pricing config.UsagePricing
	var err error
	if *builtIn {
		pricing, err = config.DefaultUsagePricing()
	} else {
		pricing, _, err = config.LoadUsagePricing(getenv(config.UsagePricingOverrideEnv))
	}
	if err != nil {
		return err
	}
	if *model == "" {
		return writeIndentedJSON(stdout, pricing)
	}
	if *model == config.InternalGovernanceModel {
		return fmt.Errorf("model %q is fixed at zero and cannot be overridden", *model)
	}
	rule, ok := pricing.Models[*model]
	if !ok {
		return fmt.Errorf("model %q is not priced by the catalog", *model)
	}
	return writeIndentedJSON(stdout, config.UsagePricingOverride{
		SchemaVersion: pricing.SchemaVersion, Models: map[string]config.ModelPricing{*model: rule},
	})
}

func pricingCheck(stdout io.Writer, getenv func(string) string) error {
	path := strings.TrimSpace(getenv(config.UsagePricingOverrideEnv))
	pricing, source, err := config.LoadUsagePricing(path)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "内置价格目录：catalog_as_of %s，共 %d 个模型\n", source.CatalogAsOf, source.ModelCount)
	switch {
	case source.Overridden():
		fmt.Fprintf(stdout, "覆盖文件：%s\n", source.OverridePath)
		if len(source.OverrideModels) > 0 {
			fmt.Fprintf(stdout, "  覆盖价格的模型：%s\n", strings.Join(source.OverrideModels, ", "))
		}
		if source.OverrideFX {
			fmt.Fprintf(stdout, "  覆盖汇率：USD/CNY %s（fx_as_of %s）\n", pricing.USDCNYRate, pricing.FXAsOf)
		}
	case path == "":
		fmt.Fprintf(stdout, "覆盖文件：未配置 %s，使用内置目录\n", config.UsagePricingOverrideEnv)
	default:
		fmt.Fprintf(stdout, "覆盖文件：%s 不存在，使用内置目录\n", path)
	}
	for _, warning := range source.Warnings {
		fmt.Fprintf(stdout, "警告：%s\n", warning)
	}
	fmt.Fprintf(stdout, "有效价格目录 sha256：%s\n", source.Digest)
	fmt.Fprintln(stdout, "校验通过")
	return nil
}

// pricingMigrateEnv converts the value of the retired GATEWAY_USAGE_PRICING_JSON
// into the minimal override file. The document goes to stdout only when an
// override is needed; the report always goes to stderr.
func pricingMigrateEnv(stdin io.Reader, stdout, stderr io.Writer) error {
	raw, err := io.ReadAll(io.LimitReader(stdin, config.MaxUsagePricingDocumentBytes+1))
	if err != nil {
		return fmt.Errorf("read legacy pricing from stdin: %w", err)
	}
	if len(raw) > config.MaxUsagePricingDocumentBytes {
		return fmt.Errorf("legacy pricing must be at most %d bytes", config.MaxUsagePricingDocumentBytes)
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return errors.New("stdin is empty; pipe the file saved by scripts/pricing.sh export-legacy into this command")
	}
	builtIn, err := config.DefaultUsagePricing()
	if err != nil {
		return err
	}
	migration, err := config.MigrateLegacyUsagePricing(builtIn, raw)
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "旧价格目录 catalog_as_of %s；内置价格目录 catalog_as_of %s\n",
		migration.LegacyCatalogAsOf, migration.BuiltInCatalogAsOf)
	if migration.LegacyOlder && len(migration.PriceChanged) > 0 {
		fmt.Fprintln(stderr, "注意：旧目录早于内置目录，下列差异可能只是过时的官方价，而不是有意调整；不需要保留的请从覆盖文件删除。")
	}
	reportModels(stderr, "价格与内置目录不同、已写入覆盖文件的模型", migration.PriceChanged)
	if migration.FXChanged {
		fmt.Fprintf(stderr, "汇率与内置目录不同、已写入覆盖文件：USD/CNY %s（fx_as_of %s）\n",
			migration.Override.USDCNYRate, migration.Override.FXAsOf)
	}
	if migration.FXDateOnly {
		fmt.Fprintln(stderr, "汇率数值与内置目录相同，仅 fx_as_of 不同：不写入覆盖文件，改用内置日期。")
	}
	reportModels(stderr, "结构已变化或含零价、无法覆盖而改用内置规则的模型（请重新核对价格，并检查“模型定价”中的覆盖价是否冲突）", migration.StructureChanged)
	reportModels(stderr, "内置目录不再定价、升级后不可用的模型", migration.NotInBuiltIn)
	reportModels(stderr, "内置目录新增、升级后对所有用户开放的模型", migration.NewInBuiltIn)
	if migration.Override == nil {
		fmt.Fprintln(stderr, "结果：无需覆盖文件；删除 .env 中的 GATEWAY_USAGE_PRICING_JSON 即可。")
		return nil
	}
	if err := writeIndentedJSON(stdout, migration.Override); err != nil {
		return err
	}
	fmt.Fprintln(stderr, "结果：覆盖文件已写到标准输出；审阅、检查后再改名为 deploy/pricing/override.json。")
	return nil
}

func reportModels(stderr io.Writer, label string, models []string) {
	if len(models) == 0 {
		return
	}
	fmt.Fprintf(stderr, "%s（%d）：%s\n", label, len(models), strings.Join(models, ", "))
}

func writeIndentedJSON(stdout io.Writer, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode pricing: %w", err)
	}
	_, err = stdout.Write(append(encoded, '\n'))
	return err
}
