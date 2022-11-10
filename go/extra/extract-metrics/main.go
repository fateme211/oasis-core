// extract-metrics extracts prometheus metrics from .go source
package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"html"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const (
	CfgMarkdown               = "markdown"
	CfgMarkdownTplFile        = "markdown.template.file"
	CfgMarkdownTplPlaceholder = "markdown.template.placeholder"
	CfgCodebasePath           = "codebase.path"
	CfgCodebaseURL            = "codebase.url"
)

var (
	scriptName = filepath.Base(os.Args[0])

	rootCmd = &cobra.Command{
		Use:   scriptName,
		Short: "Extracts Prometheus metrics from .go code.",
		Long: `This tool parses .go source files in the given codebase path
and generates a set of registered Prometheus metrics. By default it outputs JSON formatted metrics
map. You can also provide --markdown flag and it will print a Markdown-formatted table of metrics
useful for embedding into other Markdown files. Additionally, you can use --markdown.template.file
and it will embed the table in place of the placeholder in the provided template file.`,
		Example: "./extract-metrics --codebase.path ../.. --markdown",
		Run:     doExtractMetrics,
	}
)

type Metric struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Help     string   `json:"help"`
	Labels   []string `json:"labels"`
	Filename string   `json:"filename"`
	Line     int      `json:"line"`
	Vec      bool     `json:"vec"`
}

func markdownTable(metrics map[string]Metric) string {
	var ordKeys []string
	for k := range metrics {
		ordKeys = append(ordKeys, k)
	}
	sort.Slice(ordKeys, func(i, j int) bool {
		return metrics[ordKeys[i]].Name < metrics[ordKeys[j]].Name
	})

	baseDir := viper.GetString(CfgCodebasePath)
	if viper.IsSet(CfgMarkdownTplFile) && !viper.IsSet(CfgCodebaseURL) {
		baseDir = filepath.Dir(viper.GetString(CfgMarkdownTplFile))
	}

	mdTable := "Name | Type | Description | Labels | Package\n"
	mdTable += "-----|------|-------------|--------|--------\n"
	for _, k := range ordKeys {
		m := metrics[k]
		pkg, _ := filepath.Rel(viper.GetString(CfgCodebasePath), m.Filename)
		pkg = filepath.Dir(pkg)
		fileURL, _ := filepath.Rel(baseDir, m.Filename)
		if viper.IsSet(CfgCodebaseURL) {
			fileURL = viper.GetString(CfgCodebaseURL) + fileURL
		}
		desc := html.EscapeString(m.Help)
		labels := strings.Join(m.Labels, ", ")

		mdTable += fmt.Sprintf("%s | %s | %s | %s | [%s](%s)\n", m.Name, m.Type, desc,
			labels, pkg, fileURL)
	}

	return mdTable
}

func printMarkdown(metrics map[string]Metric) {
	mdTable := markdownTable(metrics)

	if !viper.IsSet(CfgMarkdownTplFile) {
		// Print Markdown table only.
		fmt.Print(mdTable)
		return
	}

	md, err := os.ReadFile(viper.GetString(CfgMarkdownTplFile))
	if err != nil {
		panic(err)
	}
	mdStr := fmt.Sprintf("---\n# DO NOT EDIT. This file was generated by %s\n---\n\n", scriptName)
	mdStr += string(md)
	mdStr = strings.Replace(mdStr, viper.GetString(CfgMarkdownTplPlaceholder)+"\n", mdTable, 1)

	fmt.Print(mdStr)
}

func printJSON(m map[string]Metric) {
	data, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s", data)
}

var metrics = map[string]Metric{}

func doExtractMetrics(cmd *cobra.Command, args []string) {
	searchDir := viper.GetString(CfgCodebasePath)
	fset := token.NewFileSet() // positions are relative to fset
	err := filepath.Walk(searchDir, func(path string, f os.FileInfo, err error) error {
		if err != nil {
			log.Fatal(err)
		}
		if f.IsDir() {
			return nil
		}
		if !strings.HasSuffix(f.Name(), ".go") {
			return nil
		}
		src, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}

		ast.Inspect(src, func(n ast.Node) bool {
			m, ok := checkNewPrometheusMetric(fset, n)
			if ok {
				m.Filename = path
				metrics[m.Name] = m
			}
			return true
		})
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	if viper.GetBool(CfgMarkdown) {
		printMarkdown(metrics)
	} else {
		printJSON(metrics)
	}
}

// checkNewPrometheusMetric checks the given node in AST, if it contains Prometheus metric.
//
// Example code in go:
//
// ```
// rhpLatency = prometheus.NewSummaryVec(
//
//	prometheus.SummaryOpts{
//	  Name: "oasis_rhp_latency",
//	  Help: "Runtime Host call latency (seconds).",
//	},
//	[]string{"call"},
//
// )
// ```
//
// Identifiers for Name and Help fields in Opts are also unwound, for example:
//
// ```
// const MetricCPUUTimeSeconds = "oasis_node_cpu_utime_seconds"
// ...
// utimeGauge = prometheus.NewGauge(
//
//	prometheus.GaugeOpts{
//		Name: MetricCPUUTimeSeconds,
//		Help: "CPU user time spent by worker as reported by /proc/<PID>/stat (seconds).",
//	},
//
// )
// ```
func checkNewPrometheusMetric(f *token.FileSet, n ast.Node) (m Metric, ok bool) {
	c, ok := n.(*ast.CallExpr)
	if !ok {
		return
	}
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "prometheus" {
		return m, false
	}
	re := regexp.MustCompile(`New(.*)`)
	if !re.MatchString(sel.Sel.String()) {
		return m, false
	}

	// Call expression is of form prometheus.New<metric Name>(...) or
	// prometheus.New<metric Name>Vec().
	m.Type = re.FindStringSubmatch(sel.Sel.String())[1]
	if strings.HasSuffix(m.Type, "Vec") {
		m.Vec = true
		m.Type = m.Type[:len(m.Type)-3]
	}

	m.Line = f.Position(c.Pos()).Line

	// Obtain metric Name and Help values.
	ast.Inspect(c.Args[0], func(n ast.Node) bool {
		// Find metrics Name: and Help: attributes.
		kv, okKV := n.(*ast.KeyValueExpr)
		if !okKV {
			return true
		}
		if kv.Key.(*ast.Ident).Name == "Name" {
			m.Name = extractValue(kv.Value)
		}
		if kv.Key.(*ast.Ident).Name == "Help" {
			m.Help = extractValue(kv.Value)
		}
		return true
	})

	// If labels are defined, extract them.
	if len(c.Args) > 1 {
		l, okL := c.Args[1].(*ast.CompositeLit)
		if !okL {
			return
		}
		for _, e := range l.Elts {
			m.Labels = append(m.Labels, extractValue(e))
		}
	}

	return
}

// extractValue returns string value of the identifier or literal.
func extractValue(n ast.Expr) string {
	lit, ok := n.(*ast.BasicLit)
	if ok {
		// Strip quotes.
		return lit.Value[1 : len(lit.Value)-1]
	}

	ident, ok := n.(*ast.Ident)
	if !ok || ident.Obj == nil {
		return ""
	}
	decl, ok := ident.Obj.Decl.(*ast.ValueSpec)
	if !ok || len(decl.Values) != 1 {
		return ""
	}
	val, ok := decl.Values[0].(*ast.BasicLit)
	if !ok {
		return ""
	}
	// Strip quotes.
	return val.Value[1 : len(val.Value)-1]
}

func main() {
	rootCmd.Flags().Bool(CfgMarkdown, false, "print metrics in markdown format")
	rootCmd.Flags().String(CfgCodebasePath, "", "path to Go codebase")
	rootCmd.Flags().String(CfgCodebaseURL, "", "show URL to Go files with this base instead of relative path (optional) (e.g. https://github.com/oasisprotocol/oasis-core/tree/master/go/)")
	rootCmd.Flags().String(CfgMarkdownTplFile, "", "path to Markdown template file")
	rootCmd.Flags().String(CfgMarkdownTplPlaceholder, "<!--- OASIS_METRICS -->", "placeholder for Markdown table in the template")
	_ = cobra.MarkFlagRequired(rootCmd.Flags(), CfgCodebasePath)
	_ = viper.BindPFlags(rootCmd.Flags())

	_ = rootCmd.Execute()
}
