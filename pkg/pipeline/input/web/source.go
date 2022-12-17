package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/livekit/egress/pkg/config"
	"github.com/livekit/egress/pkg/errors"
	"github.com/livekit/egress/pkg/pipeline/input/builder"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/tracer"
)

type errorLogger struct {
	cmd string
}

func (l *errorLogger) Write(p []byte) (int, error) {
	logger.Errorw(fmt.Sprintf("%s: %s", l.cmd, string(p)), nil)
	return len(p), nil
}

// creates a new pulse audio sink
func (s *WebInput) createPulseSink(ctx context.Context, p *config.PipelineConfig) error {
	ctx, span := tracer.Start(ctx, "WebInput.createPulseSink")
	defer span.End()

	cmd := exec.Command("pactl",
		"load-module", "module-null-sink",
		fmt.Sprintf("sink_name=\"%s\"", p.Info.EgressId),
		fmt.Sprintf("sink_properties=device.description=\"%s\"", p.Info.EgressId),
	)
	var b bytes.Buffer
	cmd.Stdout = &b
	cmd.Stderr = &errorLogger{cmd: "pactl"}
	err := cmd.Run()
	if err != nil {
		return errors.Fatal(err)
	}

	s.pulseSink = b.String()
	return nil
}

// creates a new xvfb display
func (s *WebInput) launchXvfb(ctx context.Context, p *config.PipelineConfig) error {
	ctx, span := tracer.Start(ctx, "WebInput.launchXvfb")
	defer span.End()

	dims := fmt.Sprintf("%dx%dx%d", p.Width, p.Height, p.Depth)
	logger.Debugw("launching xvfb", "display", p.Display, "dims", dims)
	xvfb := exec.Command("Xvfb", p.Display, "-screen", "0", dims, "-ac", "-nolisten", "tcp")
	xvfb.Stderr = &errorLogger{cmd: "xvfb"}
	if err := xvfb.Start(); err != nil {
		return errors.Fatal(err)
	}

	s.xvfb = xvfb
	return nil
}

// launches chrome and navigates to the url
func (s *WebInput) launchChrome(ctx context.Context, p *config.PipelineConfig) error {
	ctx, span := tracer.Start(ctx, "WebInput.launchChrome")
	defer span.End()

	logger.Debugw("launching chrome", "url", p.WebUrl)

	opts := []chromedp.ExecAllocatorOption{
		chromedp.Flag("display", p.Display),
		chromedp.Flag("window-start", "0,0"),
		chromedp.Flag("window-size", fmt.Sprintf("%d,%d", p.Width, p.Height)),
		chromedp.Env(fmt.Sprintf("PULSE_SINK=%s", p.Info.EgressId)),
	}
	for name, value := range builder.GetChromeFlags(p) {
		opts = append(opts, chromedp.Flag(name, value))
	}

	allocCtx, _ := chromedp.NewExecAllocator(context.Background(), opts...)
	chromeCtx, cancel := chromedp.NewContext(allocCtx)
	s.chromeCancel = cancel

	logEvent := func(eventType string, ev interface{ MarshalJSON() ([]byte, error) }) {
		values := make([]interface{}, 0)
		if j, err := ev.MarshalJSON(); err == nil {
			m := make(map[string]interface{})
			_ = json.Unmarshal(j, &m)
			for k, v := range m {
				values = append(values, k, v)
			}
		}
		logger.Debugw(fmt.Sprintf("chrome %s", eventType), values...)
	}

	chromedp.ListenTarget(chromeCtx, func(ev interface{}) {
		switch ev := ev.(type) {
		case *runtime.EventConsoleAPICalled:
			for _, arg := range ev.Args {
				var val interface{}
				err := json.Unmarshal(arg.Value, &val)
				if err != nil {
					continue
				}

				switch fmt.Sprint(val) {
				case builder.StartRecordingMessage:
					if s.startRecording != nil {
						select {
						case <-s.startRecording:
							continue
						default:
							close(s.startRecording)
						}
					}
				case builder.EndRecordingMessage:
					if s.endRecording != nil {
						select {
						case <-s.endRecording:
							continue
						default:
							close(s.endRecording)
						}
					}
				}
			}

			logEvent(ev.Type.String(), ev)
		case *runtime.EventExceptionThrown:
			logEvent("exception", ev)
		}
	})

	var errString string
	err := chromedp.Run(chromeCtx,
		chromedp.Navigate(p.WebUrl),
		chromedp.Evaluate(`
			if (document.querySelector('div.error')) {
				document.querySelector('div.error').innerText;
			} else {
				''
			}`, &errString,
		),
	)
	if err == nil && errString != "" {
		err = errors.New(errString)
	}
	return err
}

func (s *WebInput) updateWebUrl(p *config.PipelineConfig) error {
	if p.WebUrl != "" {
		return nil
	}

	// create start and end channels
	s.endRecording = make(chan struct{})
	if !p.CEF {
		s.startRecording = make(chan struct{})
	}

	// build input url
	inputUrl, err := url.Parse(p.TemplateBase)
	if err != nil {
		return err
	}
	values := inputUrl.Query()
	values.Set("layout", p.Layout)
	values.Set("url", p.WsUrl)
	values.Set("token", p.Token)
	inputUrl.RawQuery = values.Encode()
	p.WebUrl = inputUrl.String()
	return nil
}
