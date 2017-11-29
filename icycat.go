package main

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/puellanivis/breton/lib/files"
	"github.com/puellanivis/breton/lib/files/httpfiles"
	_ "github.com/puellanivis/breton/lib/files/plugins"
	"github.com/puellanivis/breton/lib/glog"
	flag "github.com/puellanivis/breton/lib/gnuflag"
	"github.com/puellanivis/breton/lib/util"
)

var Flags struct {
	Output    string `flag:",short=o"            desc:"Specifies which file to write the output to"`
	UserAgent string `flag:",default=allcat/1.0" desc:"Which User-Agent string to use"`

	Timeout time.Duration `flag:",default=5s"    desc:"Timeout for each read, if it expires, entire request will restart."`
}

func init() {
	flag.FlagStruct("", &Flags)
}

type Headerer interface {
	Header() (http.Header, error)
}

func PrintIcyHeaders(h Headerer) {
	header, err := h.Header()
	if err != nil {
		glog.Errorf("couldn’t get headers: %s", err)
		return
	}

	var headers []string

	for k, _ := range header {
		key := strings.ToUpper(k)

		if strings.HasPrefix(key, "ICY-") {
			headers = append(headers, k)
		}
	}
	sort.Strings(headers)

	for _, k := range headers {
		v := header[k]

		if len(v) == 1 {
			util.Statusf("%s: %q\n", k, v[0])
		} else {
			util.Statusf("%s: %q\n", k, v)
		}
	}
}

func main() {
	defer util.Init("cat", 0, 1)()

	if glog.V(2) {
		if err := flag.Set("stderrthreshold", "INFO"); err != nil {
			glog.Error(err)
		}
	}

	ctx := util.Context()
	ctx = httpfiles.WithUserAgent(ctx, Flags.UserAgent)

	args := flag.Args()

	if len(args) < 1 {
		glog.Error("icycat requires an address to stream")
		util.Exit(1)
	}

	out, err := files.Create(ctx, Flags.Output)
	if err != nil {
		glog.Fatal(err)
	}

	arg, args := args[0], args[1:]

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		f, err := files.Open(ctx, arg)
		if err != nil {
			glog.Fatal(err)
		}

		if fi, err := f.Stat(); err == nil {
			if glog.V(2) && fi.Name() != arg {
				glog.Infof("catting %s", fi.Name())
			}
		}

		if h, ok := f.(Headerer); ok {
			PrintIcyHeaders(h)
		}

		start := time.Now()
		wait := time.After(Flags.Timeout)

		n, err := files.CopyWithRunningTimeout(ctx, out, f, Flags.Timeout)
		if err != nil {
			glog.Error(err)

			if n > 0 {
				glog.Errorf("%d bytes copied in %v", n, time.Since(start))
			}
		}

		// minimum Flags.Timeout wait.
		<-wait

		if glog.V(2) {
			glog.Infof("%d bytes copied in %v", n, time.Since(start))
		}
	}
}
