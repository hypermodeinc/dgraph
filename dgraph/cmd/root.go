/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package cmd

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	checkupgrade "github.com/hypermodeinc/dgraph/v24/check_upgrade"
	"github.com/hypermodeinc/dgraph/v24/dgraph/cmd/alpha"
	"github.com/hypermodeinc/dgraph/v24/dgraph/cmd/bulk"
	"github.com/hypermodeinc/dgraph/v24/dgraph/cmd/cert"
	"github.com/hypermodeinc/dgraph/v24/dgraph/cmd/conv"
	"github.com/hypermodeinc/dgraph/v24/dgraph/cmd/debug"
	"github.com/hypermodeinc/dgraph/v24/dgraph/cmd/debuginfo"
	"github.com/hypermodeinc/dgraph/v24/dgraph/cmd/decrypt"
	"github.com/hypermodeinc/dgraph/v24/dgraph/cmd/increment"
	"github.com/hypermodeinc/dgraph/v24/dgraph/cmd/live"
	"github.com/hypermodeinc/dgraph/v24/dgraph/cmd/migrate"
	"github.com/hypermodeinc/dgraph/v24/dgraph/cmd/version"
	"github.com/hypermodeinc/dgraph/v24/dgraph/cmd/zero"
	dgraphimport "github.com/hypermodeinc/dgraph/v24/dgraph_import"
	"github.com/hypermodeinc/dgraph/v24/upgrade"
	"github.com/hypermodeinc/dgraph/v24/x"
)

// RootCmd represents the base command when called without any subcommands
var RootCmd = &cobra.Command{
	Use:   "dgraph",
	Short: "Dgraph: Distributed Graph Database",
	Long: `
Dgraph is a horizontally scalable and distributed graph database,
providing ACID transactions, consistent replication and linearizable reads.
It's built from the ground up to perform for a rich set of queries. Being a native
graph database, it tightly controls how the data is arranged on disk to optimize
for query performance and throughput, reducing disk seeks and network calls in a
cluster.
` + x.BuildDetails(),
	PersistentPreRunE: cobra.NoArgs,
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	initCmds()

	// Convinces glog that Parse() has been called to avoid noisy logs.
	// https://github.com/kubernetes/kubernetes/issues/17162#issuecomment-225596212
	x.Check(flag.CommandLine.Parse([]string{}))

	// Dumping the usage in case of an error makes the error messages harder to see.
	RootCmd.SilenceUsage = true

	x.CheckfNoLog(RootCmd.Execute())
}

var rootConf = viper.New()

// subcommands initially contains all default sub-commands.
var subcommands = []*x.SubCommand{
	&bulk.Bulk, &cert.Cert, &conv.Conv, &live.Live, &alpha.Alpha, &zero.Zero, &version.Version,
	&debug.Debug, &migrate.Migrate, &debuginfo.DebugInfo, &upgrade.Upgrade, &decrypt.Decrypt, &increment.Increment,
	&checkupgrade.CheckUpgrade, &dgraphimport.Import,
}

func initCmds() {
	RootCmd.PersistentFlags().String("cwd", "",
		"Change working directory to the path specified. The parent must exist.")
	RootCmd.PersistentFlags().String("profile_mode", "",
		"Enable profiling mode, one of [cpu, mem, mutex, block]")
	RootCmd.PersistentFlags().Int("block_rate", 0,
		"Block profiling rate. Must be used along with block profile_mode")
	RootCmd.PersistentFlags().String("config", "",
		"Configuration file. Takes precedence over default values, but is "+
			"overridden to values set with environment variables and flags.")
	RootCmd.PersistentFlags().Bool("bindall", true,
		"Use 0.0.0.0 instead of localhost to bind to all addresses on local machine.")
	RootCmd.PersistentFlags().Bool("expose_trace", false,
		"Allow trace endpoint to be accessible from remote")
	x.Check(rootConf.BindPFlags(RootCmd.PersistentFlags()))

	// Add all existing global flag (eg: from glog) to rootCmd's flags
	RootCmd.PersistentFlags().AddGoFlagSet(flag.CommandLine)

	// Always set stderrthreshold=0. Don't let users set it themselves.
	x.Check(RootCmd.PersistentFlags().Set("stderrthreshold", "0"))
	x.Check(RootCmd.PersistentFlags().MarkDeprecated("stderrthreshold",
		"Dgraph always sets this flag to 0. It can't be overwritten."))

	for _, sc := range subcommands {
		RootCmd.AddCommand(sc.Cmd)
		sc.Conf = viper.New()
		x.Check(sc.Conf.BindPFlags(sc.Cmd.Flags()))
		x.Check(sc.Conf.BindPFlags(RootCmd.PersistentFlags()))
		sc.Conf.AutomaticEnv()
		sc.Conf.SetEnvPrefix(sc.EnvPrefix)
		// Options that contain a "." should use "_" in its place when provided as an
		// environment variable.
		sc.Conf.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	}
	// For bash shell completion
	RootCmd.AddCommand(shellCompletionCmd())
	RootCmd.SetHelpTemplate(x.RootTemplate)

	cobra.OnInitialize(func() {
		// When run inside docker, the working_dir is created by root even if afterward
		// the process is run as another user. Creating a new working directory here
		// ensures that it is owned by the user instead, so it can be cleaned up without
		// requiring root when using a bind volume (i.e. a mounted host directory).
		if cwd := rootConf.GetString("cwd"); cwd != "" {
			err := os.Mkdir(cwd, 0750)
			if err != nil && !os.IsExist(err) {
				x.Fatalf("unable to create directory: %v", err)
			}
			x.CheckfNoTrace(os.Chdir(cwd))
		}

		for _, sc := range subcommands {
			// Set config file is provided for each subcommand, this is done
			// for individual subcommand because each subcommand has its own config
			// prefix, like `dgraph zero` expects the prefix to be `DGRAPH_ZERO`.
			cfg := sc.Conf.GetString("config")
			if cfg == "" {
				continue
			}
			// TODO: might want to put the rest of this scope outside the for loop, do we need to
			//       read the config file for each subcommand if there's only one global config
			//       file?
			cfgFile, err := os.OpenFile(cfg, os.O_RDONLY, 0644)
			if err != nil {
				x.Fatalf("unable to open config file for reading: %v", err)
			}
			cfgData, err := io.ReadAll(cfgFile)
			if err != nil {
				x.Fatalf("unable to read config file: %v", err)
			}
			if ext := filepath.Ext(cfg); len(ext) > 1 {
				ext = ext[1:]
				sc.Conf.SetConfigType(ext)
				var fixed io.Reader
				switch ext {
				case "json":
					fixed = convertJSON(string(cfgData))
				case "yaml", "yml":
					fixed = convertYAML(string(cfgData))
				default:
					x.Fatalf("unknown config file extension: %s", ext)
				}
				x.Check(errors.Wrapf(sc.Conf.ReadConfig(fixed), "reading config"))
			} else {
				x.Fatalf("config file requires an extension: .json or .yaml or .yml")
			}
			setGlogFlags(sc.Conf)
		}
	})
}

// setGlogFlags function sets the glog flags based on the configuration.
// We need to manually set the flags from configuration because glog reads
// values from flags, not configuration.
func setGlogFlags(conf *viper.Viper) {
	// List of flags taken from
	// https://github.com/golang/glog/blob/master/glog.go#L399
	// and https://github.com/golang/glog/blob/master/glog_file.go#L41
	glogFlags := [...]string{
		"log_dir", "logtostderr", "alsologtostderr", "v",
		"stderrthreshold", "vmodule", "log_backtrace_at",
	}
	for _, gflag := range glogFlags {
		// Set value of flag to the value in config
		stringValue := conf.GetString(gflag)
		// Special handling for log_backtrace_at flag because the flag is of
		// type tracelocation. The nil value for tracelocation type is
		// ":0"(See https://github.com/golang/glog/blob/master/glog.go#L322).
		// But we can't set nil value for the flag because of
		// https://github.com/golang/glog/blob/master/glog.go#L374
		// Skip setting value if log_backstrace_at is nil in config.
		if gflag == "log_backtrace_at" && (stringValue == "0" || stringValue == ":0") {
			continue
		}
		x.Check(flag.Lookup(gflag).Value.Set(stringValue))
	}
}

func shellCompletionCmd() *cobra.Command {

	cmd := &cobra.Command{

		Use:         "completion",
		Short:       "Generates shell completion scripts for bash or zsh",
		Annotations: map[string]string{"group": "tool"},
	}
	cmd.SetHelpTemplate(x.NonRootTemplate)

	// bash subcommand
	cmd.AddCommand(&cobra.Command{
		Use:   "bash",
		Short: "bash shell completion",
		Long: `To load bash completion run:
dgraph completion bash > dgraph-completion.sh

To configure your bash shell to load completions for each session,
add to your bashrc:

# ~/.bashrc or ~/.profile
. path/to/dgraph-completion.sh
`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return RootCmd.GenBashCompletion(os.Stdout)
		},
	})

	// zsh subcommand
	cmd.AddCommand(&cobra.Command{
		Use:   "zsh",
		Short: "zsh shell completion",
		Long: `To generate zsh completion run:
dgraph completion zsh > _dgraph

Then install the completion file somewhere in your $fpath or
$_compdir paths. You must enable the compinit and compinstall plugins.

For more information, see the official Zsh docs:
http://zsh.sourceforge.net/Doc/Release/Completion-System.html
`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return RootCmd.GenZshCompletion(os.Stdout)
		},
	})

	return cmd

}

// convertJSON converts JSON hierarchical config objects into a flattened map fulfilling the
// z.SuperFlag string format so that Viper can correctly set z.SuperFlag config options for the
// respective subcommands. If JSON hierarchical config objects are not used, convertJSON doesn't
// change anything and returns the config file string as it is. For example:
//
//	{
//	  "mutations": "strict",
//	  "badger": {
//	    "compression": "zstd:1",
//	    "goroutines": 5
//	  },
//	  "raft": {
//	    "idx": 2,
//	    "learner": true
//	  },
//	  "security": {
//	    "whitelist": "127.0.0.1,0.0.0.0"
//	  }
//	}
//
// Is converted into:
//
//	{
//	  "mutations": "strict",
//	  "badger": "compression=zstd:1; goroutines=5;",
//	  "raft": "idx=2; learner=true;",
//	  "security": "whitelist=127.0.0.1,0.0.0.0;"
//	}
//
// Viper then uses the "converted" JSON to set the z.SuperFlag strings in subcommand option structs.
func convertJSON(old string) io.Reader {
	dec := json.NewDecoder(strings.NewReader(old))
	config := make(map[string]interface{})
	x.Panic(dec.Decode(&config))

	// super holds superflags to later be condensed into 'good'
	super, good := make(map[string]map[string]interface{}), make(map[string]string)
	for k, v := range config {
		switch t := v.(type) {
		case map[string]interface{}:
			super[k] = t
		default:
			good[k] = fmt.Sprintf("%v", v)
		}
	}
	// condense superflags
	for f, options := range super {
		for k, v := range options {
			// JSON does not have distinct types for integers and floats.
			// Go will always give us a float64 value. So, an exceptionally
			// large integer like 1_000_000 will be printed as 1e06 unless
			// we format it carefully.
			if vFloat, ok := v.(float64); ok {
				v = strconv.FormatFloat(vFloat, 'f', -1, 64)
			}
			good[f] += fmt.Sprintf("%s=%v; ", k, v)
		}
		good[f] = good[f][:len(good[f])-1]
	}
	// generate good json string
	buf := &bytes.Buffer{}
	enc := json.NewEncoder(buf)
	enc.SetIndent("", "    ")
	x.Panic(enc.Encode(&good))

	return buf
}

// convertYAML converts YAML hierarchical notation into a flattened map fulfilling the z.SuperFlag
// string format so that Viper can correctly set the z.SuperFlag config options for the respective
// subcommands. If YAML hierarchical notation is not used, convertYAML doesn't change anything and
// returns the config file string as it is. For example:
//
//	mutations: strict
//	badger:
//	  compression: zstd:1
//	  goroutines: 5
//	raft:
//	  idx: 2
//	  learner: true
//	security:
//	  whitelist: "127.0.0.1,0.0.0.0"
//
// Is converted into:
//
//	mutations: strict
//	badger: "compression=zstd:1; goroutines=5;"
//	raft: "idx=2; learner=true;"
//	security: "whitelist=127.0.0.1,0.0.0.0;"
//
// Viper then uses the "converted" YAML to set the z.SuperFlag strings in subcommand option structs.
func convertYAML(old string) io.Reader {
	isFlat := func(l string) bool {
		if len(l) < 1 {
			return false
		}
		if unicode.IsSpace(rune(l[0])) {
			return false
		}
		return true
	}
	isOption := func(l string) bool {
		if len(l) < 3 {
			return false
		}
		if !strings.Contains(l, ":") {
			return false
		}
		if !unicode.IsSpace(rune(l[0])) {
			return false
		}
		return true
	}
	isSuper := func(l string) bool {
		s := strings.TrimSpace(l)
		if len(s) < 1 {
			return false
		}
		if s[len(s)-1] != ':' {
			return false
		}
		return true
	}
	getName := func(l string) string {
		s := strings.TrimSpace(l)
		return s[:strings.IndexRune(s, rune(':'))]
	}
	getValue := func(l string) string {
		s := strings.TrimSpace(l)
		v := s[strings.IndexRune(s, rune(':'))+2:]
		return strings.ReplaceAll(v, `"`, ``)
	}
	super, good, last := make(map[string]string), make([]string, 0), ""
	for _, line := range strings.Split(old, "\n") {
		if isSuper(line) {
			last = getName(line)
			continue
		}
		if isOption(line) {
			name, value := getName(line), getValue(line)
			super[last] += name + "=" + value + "; "
			continue
		}
		if isFlat(line) {
			good = append(good, strings.TrimSpace(line))
		}
	}
	for k, v := range super {
		super[k] = `"` + strings.TrimSpace(v) + `"`
		good = append(good, fmt.Sprintf("%s: %s", k, super[k]))
	}
	return strings.NewReader(strings.Join(good, "\n"))
}
