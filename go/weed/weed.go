package main

import (
	"code.google.com/p/weed-fs/go/glog"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/template"
	"time"
	"unicode"
	"unicode/utf8"
)

var IsDebug *bool
var server *string

var commands = []*Command{
	cmdCompact,
	cmdFix,
	cmdMaster,
	cmdUpload,
	cmdDownload,
	cmdShell,
	cmdVersion,
	cmdVolume,
	cmdExport,
}

var exitStatus = 0
var exitMu sync.Mutex

func setExitStatus(n int) {
	exitMu.Lock()
	if exitStatus < n {
		exitStatus = n
	}
	exitMu.Unlock()
}

func main() {
	glog.ToStderrAndLog()
	glog.MaxSize = 1024 * 1024 * 32
	rand.Seed(time.Now().UnixNano())
	flag.Usage = usage
	flag.Parse()

	args := flag.Args()
	if len(args) < 1 {
		usage()
	}

	if args[0] == "help" {
		help(args[1:])
		for _, cmd := range commands {
			if len(args) >= 2 && cmd.Name() == args[1] && cmd.Run != nil {
				fmt.Fprintf(os.Stderr, "Default Parameters:\n")
				cmd.Flag.PrintDefaults()
			}
		}
		return
	}

	for _, cmd := range commands {
		if cmd.Name() == args[0] && cmd.Run != nil {
			cmd.Flag.Usage = func() { cmd.Usage() }
			cmd.Flag.Parse(args[1:])
			args = cmd.Flag.Args()
			IsDebug = cmd.IsDebug
			if !cmd.Run(cmd, args) {
				fmt.Fprintf(os.Stderr, "\n")
				cmd.Flag.Usage()
				fmt.Fprintf(os.Stderr, "Default Parameters:\n")
				cmd.Flag.PrintDefaults()
			}
			exit()
			return
		}
	}

	fmt.Fprintf(os.Stderr, "weed: unknown subcommand %q\nRun 'weed help' for usage.\n", args[0])
	setExitStatus(2)
	exit()
}

var usageTemplate = `WeedFS is a software to store billions of files and serve them fast!

Usage:

	weed command [arguments]

The commands are:
{{range .}}{{if .Runnable}}
    {{.Name | printf "%-11s"}} {{.Short}}{{end}}{{end}}

Use "weed help [command]" for more information about a command.

`

var helpTemplate = `{{if .Runnable}}Usage: weed {{.UsageLine}}
{{end}}
  {{.Long}}
`

// tmpl executes the given template text on data, writing the result to w.
func tmpl(w io.Writer, text string, data interface{}) {
	t := template.New("top")
	t.Funcs(template.FuncMap{"trim": strings.TrimSpace, "capitalize": capitalize})
	template.Must(t.Parse(text))
	if err := t.Execute(w, data); err != nil {
		panic(err)
	}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	r, n := utf8.DecodeRuneInString(s)
	return string(unicode.ToTitle(r)) + s[n:]
}

func printUsage(w io.Writer) {
	tmpl(w, usageTemplate, commands)
}

func usage() {
	printUsage(os.Stderr)
	os.Exit(2)
}

// help implements the 'help' command.
func help(args []string) {
	if len(args) == 0 {
		printUsage(os.Stdout)
		// not exit 2: succeeded at 'weed help'.
		return
	}
	if len(args) != 1 {
		fmt.Fprintf(os.Stderr, "usage: weed help command\n\nToo many arguments given.\n")
		os.Exit(2) // failed at 'weed help'
	}

	arg := args[0]

	for _, cmd := range commands {
		if cmd.Name() == arg {
			tmpl(os.Stdout, helpTemplate, cmd)
			// not exit 2: succeeded at 'weed help cmd'.
			return
		}
	}

	fmt.Fprintf(os.Stderr, "Unknown help topic %#q.  Run 'weed help'.\n", arg)
	os.Exit(2) // failed at 'weed help cmd'
}

var atexitFuncs []func()

func atexit(f func()) {
	atexitFuncs = append(atexitFuncs, f)
}

func exit() {
	for _, f := range atexitFuncs {
		f()
	}
	os.Exit(exitStatus)
}

func exitIfErrors() {
	if exitStatus != 0 {
		exit()
	}
}

func writeJson(w http.ResponseWriter, r *http.Request, obj interface{}) (err error) {
	w.Header().Set("Content-Type", "application/javascript")
	var bytes []byte
	if r.FormValue("pretty") != "" {
		bytes, err = json.MarshalIndent(obj, "", "  ")
	} else {
		bytes, err = json.Marshal(obj)
	}
	if err != nil {
		return
	}
	callback := r.FormValue("callback")
	if callback == "" {
		_, err = w.Write(bytes)
	} else {
		if _, err = w.Write([]uint8(callback)); err != nil {
			return
		}
		if _, err = w.Write([]uint8("(")); err != nil {
			return
		}
		fmt.Fprint(w, string(bytes))
		if _, err = w.Write([]uint8(")")); err != nil {
			return
		}
	}
	return
}

// wrapper for writeJson - just logs errors
func writeJsonQuiet(w http.ResponseWriter, r *http.Request, obj interface{}) {
	if err := writeJson(w, r, obj); err != nil {
		glog.V(0).Infof("error writing JSON %s: %s", obj, err.Error())
	}
}
func writeJsonError(w http.ResponseWriter, r *http.Request, err error) {
	m := make(map[string]interface{})
	m["error"] = err.Error()
	writeJsonQuiet(w, r, m)
}

func debug(params ...interface{}) {
	if *IsDebug {
		glog.V(0).Infoln(params)
	}
}
func secure(whiteList []string, f func(w http.ResponseWriter, r *http.Request)) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(whiteList) == 0 {
			f(w, r)
			return
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			for _, ip := range whiteList {
				if ip == host {
					f(w, r)
					return
				}
			}
		}
		writeJsonQuiet(w, r, map[string]interface{}{"error": "No write permisson from " + host})
	}
}

func parseURLPath(path string) (vid, fid, filename, ext string, isVolumeIdOnly bool) {
	switch strings.Count(path, "/") {
	case 3:
		parts := strings.Split(path, "/")
		vid, fid, filename = parts[1], parts[2], parts[3]
		ext = filepath.Ext(filename)
	case 2:
		parts := strings.Split(path, "/")
		vid, fid = parts[1], parts[2]
		dotIndex := strings.LastIndex(fid, ".")
		if dotIndex > 0 {
			ext = fid[dotIndex:]
			fid = fid[0:dotIndex]
		}
	default:
		sepIndex := strings.LastIndex(path, "/")
		commaIndex := strings.LastIndex(path[sepIndex:], ",")
		if commaIndex <= 0 {
			vid, isVolumeIdOnly = path[sepIndex+1:], true
			return
		}
		dotIndex := strings.LastIndex(path[sepIndex:], ".")
		vid = path[sepIndex+1 : commaIndex]
		fid = path[commaIndex+1:]
		ext = ""
		if dotIndex > 0 {
			fid = path[commaIndex+1 : dotIndex]
			ext = path[dotIndex:]
		}
	}
	return
}
