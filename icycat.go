package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"

	"github.com/puellanivis/breton/lib/files"
	"github.com/puellanivis/breton/lib/files/httpfiles"
	_ "github.com/puellanivis/breton/lib/files/plugins"
	"github.com/puellanivis/breton/lib/files/socketfiles"
	"github.com/puellanivis/breton/lib/glog"
	flag "github.com/puellanivis/breton/lib/gnuflag"
	"github.com/puellanivis/breton/lib/io/bufpipe"
	"github.com/puellanivis/breton/lib/metrics"
	_ "github.com/puellanivis/breton/lib/metrics/http"
	"github.com/puellanivis/breton/lib/mpeg/framer"
	"github.com/puellanivis/breton/lib/mpeg/ts"
	"github.com/puellanivis/breton/lib/mpeg/ts/dvb"
	"github.com/puellanivis/breton/lib/mpeg/ts/psi"
	"github.com/puellanivis/breton/lib/os/process"
)

// Version information ready for build-time injection.
var (
	Version    = "v2.0.0"
	Buildstamp = "dev"
)

// Flags contains all of the flags defined for the application.
var Flags struct {
	Output    string `flag:",short=o"            desc:"Specifies which file to write the output to"`
	UserAgent string `flag:",default=icycat/2.0" desc:"Which User-Agent string to use"`
	Quiet     bool   `flag:",short=q"            desc:"If set, supresses output from subprocesses."`

	// --packet-size defaults to 1316, which is 1500 - (1500 mod 188)
	// Where 1500 is the typical ethernet MTU, and 188 is the mpegts packet size.
	PacketSize int `flag:",default=1316"         desc:"If outputing to udp, default to using this packet size."`

	Timeout time.Duration `flag:",default=5s"    desc:"The timeout between rapid copy errors."`

	Metrics        bool   `desc:"If set, publish metrics to the given metrics-port or metrics-addr."`
	MetricsPort    int    `desc:"Which port to publish metrics with. (default auto-assign)"`
	MetricsAddress string `desc:"Which local address to listen on; overrides metrics-port flag."`
}

func init() {
	flag.Struct("", &Flags)
}

var (
	bwLifetime = metrics.Gauge("bandwidth_lifetime_bps", "bandwidth of the copy to output process (bits/second)")
	bwRunning  = metrics.Gauge("bandwidth_running_bps", "bandwidth of the copy to output process (bits/second)")
)

type headerer interface {
	Header() (http.Header, error)
}

func printIcyHeaders(h headerer) (name string) {
	header, err := h.Header()
	if err != nil {
		glog.Errorf("couldn’t get headers: %s", err)
		return ""
	}

	var headers []string

	for k := range header {
		key := strings.ToUpper(k)

		if strings.HasPrefix(key, "ICY-") {
			headers = append(headers, k)
		}
	}
	sort.Strings(headers)

	for _, key := range headers {
		val := header[key]

		var v interface{} = val
		if len(val) == 1 {
			v = val[0]
		}

		glog.Infof("%s: %q\n", key, v)
	}

	return header.Get("Icy-Name")
}

var (
	stderr = os.Stderr
	mux    *ts.Mux
)

type discontinuityMarker interface {
	Discontinuity()
}

func openOutput(ctx context.Context, filename string) (io.WriteCloser, func(), error) {
	discontinuity := func() {}

	if !strings.HasPrefix(filename, "udp:") && !strings.HasPrefix(filename, "mpegts:") {
		f, err := files.Create(ctx, filename)
		if err != nil {
			return nil, nil, err
		}

		glog.Infof("output: %s", f.Name())
		return f, discontinuity, nil
	}

	filename = strings.TrimPrefix(filename, "mpegts:")

	uri, err := url.Parse(filename)
	if err != nil {
		return nil, nil, err
	}

	var opts []files.Option

	if uri.Scheme == "udp" {
		// Default packet size: what the flag --packet-size is.
		pktSize := Flags.PacketSize

		q := uri.Query()
		if urlPktSize := q.Get(socketfiles.FieldPacketSize); urlPktSize != "" {
			// If the output URL has a urlPktSize value, override the default.
			sz, err := strconv.ParseInt(urlPktSize, 0, strconv.IntSize)
			if err != nil {
				return nil, nil, errors.Errorf("bad %s value: %s: %+v", socketfiles.FieldPacketSize, urlPktSize, err)
			}

			pktSize = int(sz)
		}

		// Our packet size needs to be an integer multiple of the mpegts packet size.
		pktSize -= (pktSize % ts.PacketSize)

		// Our packet size needs to be at least the mpegts packet size.
		if pktSize <= 0 {
			pktSize = ts.PacketSize
		}

		q.Set(socketfiles.FieldPacketSize, fmt.Sprint(pktSize))

		uri.RawQuery = q.Encode()
		filename = uri.String()

		opts = append(opts, socketfiles.WithIgnoreErrors(true))
	}

	f, err := files.Create(ctx, filename, opts...)
	if err != nil {
		return nil, nil, err
	}
	glog.Infof("output: %s", f.Name())

	mux = ts.NewMux(f)

	var wg sync.WaitGroup

	wr, err := mux.Writer(ctx, 1, ts.ProgramTypeAudio)
	if err != nil {
		f.Close()
		return nil, nil, err
	}

	if s, ok := wr.(discontinuityMarker); ok {
		discontinuity = s.Discontinuity
	}

	pipe := bufpipe.New(ctx)
	s := framer.NewScanner(pipe)

	wg.Add(1)
	go func() {
		defer func() {
			if err := wr.Close(); err != nil {
				glog.Errorf("mux.Writer.Close: %+v", err)
			}

			wg.Done()
		}()

		for s.Scan() {
			b := s.Bytes()

			n, err := wr.Write(b)
			if err != nil {
				glog.Errorf("mux.Writer.Write: %+v", err)
				return
			}

			if n < len(b) {
				glog.Errorf("mux.Writer.Write: %+v", io.ErrShortWrite)
			}
		}

		if err := s.Err(); err != nil {
			glog.Errorf("framer.Scanner: %s: %+v", filename, err)
		}
	}()

	out := newTriggerWriter(pipe)

	go func() {
		<-out.Trigger()
		for err := range mux.Serve(ctx) {
			glog.Fatalf("mux.Serve: %+v", err)
		}
	}()

	go func() {
		wg.Wait()
		for err := range mux.Close() {
			glog.Errorf("mux.Close: %+v", err)
		}
	}()

	return out, discontinuity, nil
}

// DVBService sets the dvb.ServiceDescriptor to be used by the muxer.
func DVBService(desc *dvb.ServiceDescriptor) {
	if mux != nil {
		service := &dvb.Service{
			ID: 0x0001,
		}
		service.Descriptors = append(service.Descriptors, desc)

		sdt := &dvb.ServiceDescriptorTable{
			Syntax: &psi.SectionSyntax{
				TableIDExtension: 1,
				Current:          true,
			},
			OriginalNetworkID: 0xFF01,
			Services:          []*dvb.Service{service},
		}
		mux.SetDVBSDT(sdt)

		switch {
		case glog.V(5) == true:
			glog.Infof("dvb.sdt: %v", sdt)

		case glog.V(2) == true:
			glog.Infof("DVB Service Description: %v", desc)
		}
	}
}

// ICECASTReader returns an io.Reader from the given filename that reads an ICECAST stream.
func ICECASTReader(ctx context.Context, filename string, discontinuity func()) (io.Reader, error) {
	reopen := func() (files.Reader, error) {
		discontinuity()

		// BUG: if you attempt to load a SHOUTcast 1.9.x address,
		// it will return an HTTP version field of "ICY" not "HTTP/x.y",
		// and Go’s net/http library will barf and return an error.
		// There is no way at this time to tell it to treat said HTTP version as "HTTP/1.0"
		// without possibly hijacking the stream through a text transform that looks to see if it
		// starts with ICY, and replaces that with HTTP/1.0…
		//
		// BETTER: net/http should allow one to say "ICY" maps to HTTP/1.0,
		// it already has short-circuits for "HTTP/1.0" and "HTTP/1.1" after all.
		f, err := files.Open(ctx, filename)
		if err != nil {
			return nil, err
		}

		if glog.V(2) && f.Name() != filename {
			glog.Infof("catting %s", f.Name())
		}

		return f, err
	}

	f, err := reopen()
	if err != nil {
		return nil, err
	}

	if h, ok := f.(headerer); ok {
		name := printIcyHeaders(h)
		if name == "" {
			name = f.Name()
		}

		ServiceDesc := &dvb.ServiceDescriptor{
			Type:     dvb.ServiceTypeRadio,
			Provider: "icycat",
			Name:     name,
		}

		DVBService(ServiceDesc)
	}

	opts := []files.CopyOption{
		files.WithWatchdogTimeout(Flags.Timeout),
	}

	pipe := bufpipe.New(ctx)

	go func() {
		defer pipe.Close()

		for {
			start := time.Now()
			wait := time.After(Flags.Timeout)

			if glog.V(1) {
				glog.Infof("copying to buffer: %s", f.Name())
			}

			n, err := files.Copy(ctx, pipe, f, opts...)

			// We reopen in every loop, so after files.Copy, we have to Close it.
			if err2 := f.Close(); err == nil {
				err = err2
			}

			if err != nil {
				glog.Error(err)

				if n > 0 {
					glog.Errorf("%d bytes copied in %v", n, time.Since(start))
				}

			} else if glog.V(2) {
				glog.Infof("%d bytes copied in %v", n, time.Since(start))
			}

			select {
			case <-wait:
			case <-ctx.Done():
				return
			}

			f, err = reopen()
			if err != nil {
				glog.Errorf("%+v", err)
			}
		}
	}()

	return pipe, nil
}

func main() {
	ctx, finish := process.Init("icycat", Version, Buildstamp)
	defer finish()

	ctx = httpfiles.WithUserAgent(ctx, Flags.UserAgent)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	args := flag.Args()
	if len(args) < 1 {
		flag.Usage()
		process.Exit(1)
	}

	if Flags.Quiet {
		stderr = nil
	}

	if glog.V(2) {
		if err := flag.Set("stderrthreshold", "INFO"); err != nil {
			glog.Error(err)
		}
	}

	if Flags.MetricsPort != 0 || Flags.MetricsAddress != "" {
		Flags.Metrics = true
	}

	if Flags.Metrics {
		go func() {
			addr := Flags.MetricsAddress
			if addr == "" {
				addr = fmt.Sprintf(":%d", Flags.MetricsPort)
			}

			l, err := net.Listen("tcp", addr)
			if err != nil {
				glog.Fatal("net.Listen: ", err)
			}

			msg := fmt.Sprintf("metrics available at: http://%s/metrics", l.Addr())
			fmt.Fprintln(os.Stderr, msg)
			glog.Info(msg)

			http.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
				http.Redirect(w, req, "/metrics", http.StatusMovedPermanently)
			})

			srv := &http.Server{}

			go func() {
				<-ctx.Done()

				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()

				srv.Shutdown(ctx)

				l.Close()
			}()

			if err := srv.Serve(l); err != nil {
				if err != http.ErrServerClosed {
					glog.Fatal("http.Serve: ", err)
				}
			}
		}()
	}

	out, discontinuity, err := openOutput(ctx, Flags.Output)
	if err != nil {
		glog.Fatal(err)
	}
	defer func() {
		if err := out.Close(); err != nil {
			glog.Error(err)
		}
	}()

	arg, args := args[0], args[1:]

	var opts []files.CopyOption

	if Flags.Metrics {
		opts = append(opts,
			files.WithMetricsScale(8), // bits instead of bytes
			files.WithBandwidthMetrics(bwLifetime),
			files.WithIntervalBandwidthMetrics(bwRunning, 10, 1*time.Second),
		)
	}

	in, err := ICECASTReader(ctx, arg, discontinuity)
	if err != nil {
		glog.Fatalf("ICECASTReader: %+v", err)
	}

	for {
		select {
		case <-ctx.Done():
			glog.Error(ctx.Err())
			return
		default:
		}

		start := time.Now()
		wait := time.After(Flags.Timeout)

		n, err := files.Copy(ctx, out, in, opts...)

		if err != nil && err != io.EOF {
			glog.Error(err)

			if n > 0 {
				glog.Errorf("%d bytes copied in %v", n, time.Since(start))
			}

		} else if glog.V(2) {
			glog.Infof("%d bytes copied in %v", n, time.Since(start))
		}

		if err == io.EOF {
			break
		}

		// minimum Flags.Timeout wait.
		select {
		case <-wait:
		case <-ctx.Done():
			glog.Error(ctx.Err())
			return
		}
	}
}
