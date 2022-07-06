package source

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/tracer"

	"github.com/livekit/egress/pkg/config"
	"github.com/livekit/egress/pkg/errors"
	"github.com/livekit/egress/pkg/pipeline/params"

	lksdk "github.com/livekit/server-sdk-go"
)

const (
	startRecordingLog = "START_RECORDING"
	endRecordingLog   = "END_RECORDING"
)

type layoutUpdate struct {
	Layout string `json:"layout"`
}

type WebSource struct {
	pulseSink    string
	xvfb         *exec.Cmd
	chromeCancel context.CancelFunc
	roomService  *lksdk.RoomServiceClient
	roomId       string

	startRecording chan struct{}
	endRecording   chan struct{}

	logger logger.Logger
}

func init() {
	rand.Seed(time.Now().UnixNano())
}

func NewWebSource(ctx context.Context, conf *config.Config, p *params.Params) (*WebSource, error) {
	ctx, span := tracer.Start(ctx, "WebSource.New")
	defer span.End()

	s := &WebSource{
		endRecording: make(chan struct{}),
		roomId:       p.Info.RoomId,
		logger:       p.Logger,
	}

	var inputUrl string
	if p.CustomInputURL != "" {
		inputUrl = p.CustomInputURL
	} else {
		s.startRecording = make(chan struct{})
		inputUrl = fmt.Sprintf(
			"%s?layout=%s&url=%s&token=%s",
			p.TemplateBase, p.Layout, url.QueryEscape(p.LKUrl), p.Token,
		)
	}

	if err := s.createAudioSink(ctx, p.Info.EgressId); err != nil {
		s.logger.Errorw("failed to load pulse sink", err)
		return nil, err
	}

	if err := s.launchXvfb(ctx, p.Display, p.Width, p.Height, p.Depth); err != nil {
		s.logger.Errorw("failed to launch xvfb", err)
		s.Close()
		return nil, err
	}

	if err := s.launchChrome(ctx, inputUrl, p.Info.EgressId, p.Display, p.Width, p.Height, conf.Insecure); err != nil {
		s.logger.Errorw("failed to launch chrome", err, "display", p.Display)
		s.Close()
		return nil, err
	}

	s.roomService = lksdk.NewRoomServiceClient(p.LKUrl, conf.ApiKey, conf.ApiSecret)

	return s, nil
}

// creates a new pulse audio sink
func (s *WebSource) createAudioSink(ctx context.Context, egressID string) error {
	ctx, span := tracer.Start(ctx, "WebSource.createAudioSink")
	defer span.End()

	cmd := exec.Command("pactl",
		"load-module", "module-null-sink",
		fmt.Sprintf("sink_name=\"%s\"", egressID),
		fmt.Sprintf("sink_properties=device.description=\"%s\"", egressID),
	)
	var b bytes.Buffer
	cmd.Stdout = &b
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		return err
	}

	s.pulseSink = b.String()
	return nil
}

// creates a new xvfb display
func (s *WebSource) launchXvfb(ctx context.Context, display string, width, height, depth int32) error {
	ctx, span := tracer.Start(ctx, "WebSource.launchXvfb")
	defer span.End()

	dims := fmt.Sprintf("%dx%dx%d", width, height, depth)
	s.logger.Debugw("launching xvfb", "display", display, "dims", dims)
	xvfb := exec.Command("Xvfb", display, "-screen", "0", dims, "-ac", "-nolisten", "tcp")
	if err := xvfb.Start(); err != nil {
		return err
	}
	s.xvfb = xvfb
	return nil
}

// launches chrome and navigates to the url
func (s *WebSource) launchChrome(ctx context.Context, url, egressID, display string, width, height int32, insecure bool) error {
	ctx, span := tracer.Start(ctx, "WebSource.launchChrome")
	defer span.End()

	s.logger.Debugw("launching chrome", "url", url)

	opts := []chromedp.ExecAllocatorOption{
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.DisableGPU,
		chromedp.NoSandbox,

		// puppeteer default behavior
		chromedp.Flag("disable-infobars", true),
		chromedp.Flag("excludeSwitches", "enable-automation"),
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("enable-features", "NetworkService,NetworkServiceInProcess"),
		chromedp.Flag("disable-background-timer-throttling", true),
		chromedp.Flag("disable-backgrounding-occluded-windows", true),
		chromedp.Flag("disable-breakpad", true),
		chromedp.Flag("disable-client-side-phishing-detection", true),
		chromedp.Flag("disable-default-apps", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-features", "site-per-process,TranslateUI,BlinkGenPropertyTrees"),
		chromedp.Flag("disable-hang-monitor", true),
		chromedp.Flag("disable-ipc-flooding-protection", true),
		chromedp.Flag("disable-popup-blocking", true),
		chromedp.Flag("disable-prompt-on-repost", true),
		chromedp.Flag("disable-renderer-backgrounding", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("force-color-profile", "srgb"),
		chromedp.Flag("metrics-recording-only", true),
		chromedp.Flag("safebrowsing-disable-auto-update", true),
		chromedp.Flag("password-store", "basic"),
		chromedp.Flag("use-mock-keychain", true),

		// custom args
		chromedp.Flag("kiosk", true),
		chromedp.Flag("enable-automation", false),
		chromedp.Flag("autoplay-policy", "no-user-gesture-required"),
		chromedp.Flag("window-position", "0,0"),
		chromedp.Flag("window-size", fmt.Sprintf("%d,%d", width, height)),

		// output
		chromedp.Env(fmt.Sprintf("PULSE_SINK=%s", egressID)),
		chromedp.Flag("display", display),
	}

	if insecure {
		opts = append(opts,
			chromedp.Flag("disable-web-security", true),
			chromedp.Flag("allow-running-insecure-content", true),
		)
	}

	allocCtx, _ := chromedp.NewExecAllocator(context.Background(), opts...)
	chromeCtx, cancel := chromedp.NewContext(allocCtx)
	s.chromeCancel = cancel

	chromedp.ListenTarget(chromeCtx, func(ev interface{}) {
		switch ev := ev.(type) {
		case *runtime.EventConsoleAPICalled:
			args := make([]string, 0, len(ev.Args))
			for _, arg := range ev.Args {
				var val interface{}
				err := json.Unmarshal(arg.Value, &val)
				if err != nil {
					continue
				}
				msg := fmt.Sprint(val)
				args = append(args, msg)
				if msg == startRecordingLog {
					select {
					case <-s.startRecording:
						continue
					default:
						close(s.startRecording)
					}
				} else if msg == endRecordingLog {
					select {
					case <-s.endRecording:
						continue
					default:
						close(s.endRecording)
					}
				}
			}
			s.logger.Debugw(fmt.Sprintf("chrome %s: %s", ev.Type.String(), strings.Join(args, " ")))
		}
	})

	var errString string
	err := chromedp.Run(chromeCtx,
		chromedp.Navigate(url),
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

func (s *WebSource) UpdateLayout(ctx context.Context, layout string) error {
	update := layoutUpdate{
		Layout: layout,
	}

	msg, err := json.Marshal(update)
	if err != nil {
		return err
	}

	req := &livekit.SendDataRequest{
		Room: s.roomId,
		Data: msg,
		Kind: livekit.DataPacket_RELIABLE,
	}

	if _, err := s.roomService.SendData(ctx, req); err != nil {
		return err
	}

	return nil
}

func (s *WebSource) StartRecording() chan struct{} {
	return s.startRecording
}

func (s *WebSource) EndRecording() chan struct{} {
	return s.endRecording
}

func (s *WebSource) Close() {
	if s.chromeCancel != nil {
		s.chromeCancel()
		s.chromeCancel = nil
	}

	if s.xvfb != nil {
		err := s.xvfb.Process.Signal(os.Interrupt)
		if err != nil {
			s.logger.Errorw("failed to kill xvfb", err)
		}
		s.xvfb = nil
	}

	if s.pulseSink != "" {
		err := exec.Command("pactl", "unload-module", s.pulseSink).Run()
		if err != nil {
			s.logger.Errorw("failed to unload pulse sink", err)
		}
	}
}
