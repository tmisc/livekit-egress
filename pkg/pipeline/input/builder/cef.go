package builder

import (
	"fmt"
	"os"
	"strings"

	"github.com/tinyzimmer/go-gst/gst"

	"github.com/livekit/egress/pkg/config"
)

type CEFInput struct {
	elements []*gst.Element
	demux    *gst.Element
}

func NewCEFInput(p *config.PipelineConfig) (*CEFInput, error) {
	if err := os.Setenv("DISPLAY", p.Display); err != nil {
		return nil, err
	}

	cefSrc, err := gst.NewElement("cefsrc")
	if err != nil {
		return nil, err
	}

	if err = cefSrc.SetProperty("url", p.WebUrl); err != nil {
		return nil, err
	}
	if err = cefSrc.SetProperty("await-console-message", StartRecordingMessage); err != nil {
		return nil, err
	}

	flags := GetChromeFlags(p)
	flagList := make([]string, 0, len(flags)*2)
	for name, value := range flags {
		switch v := value.(type) {
		case bool:
			flagList = append(flagList, name)
		case string:
			flagList = append(flagList, fmt.Sprintf("%s=%s", name, strings.Replace(v, ",", "|", -1)))
		}
	}
	if err = cefSrc.SetProperty("chrome-extra-flags", strings.Join(flagList, ",")); err != nil {
		return nil, err
	}

	caps, err := gst.NewElement("capsfilter")
	if err != nil {
		return nil, err
	}
	if err = caps.SetProperty("caps", gst.NewCapsFromString(
		fmt.Sprintf("video/x-raw,width=%d,height=%d,framerate=%d/1", p.Width, p.Height, p.Framerate),
	)); err != nil {
		return nil, err
	}

	cefDemux, err := gst.NewElement("cefdemux")
	if err != nil {
		return nil, err
	}

	return &CEFInput{
		elements: []*gst.Element{cefSrc, caps, cefDemux},
		demux:    cefDemux,
	}, nil
}

func (c *CEFInput) AddToBin(bin *gst.Bin) error {
	return bin.AddMany(c.elements...)
}

func (c *CEFInput) Link() error {
	return gst.ElementLinkMany(c.elements...)
}

func GetChromeFlags(p *config.PipelineConfig) map[string]interface{} {
	flags := map[string]interface{}{
		"no-first-run":             true,
		"no-default-browser-check": true,
		"disable-gpu":              true,
		"no-sandbox":               true,
		// puppeteer default behavior
		"disable-infobars":                       true,
		"excludeSwitches":                        "enable-automation",
		"disable-background-networking":          true,
		"enable-features":                        "NetworkService,NetworkServiceInProcess",
		"disable-background-timer-throttling":    true,
		"disable-backgrounding-occluded-windows": true,
		"disable-breakpad":                       true,
		"disable-client-side-phishing-detection": true,
		"disable-default-apps":                   true,
		"disable-dev-shm-usage":                  true,
		"disable-extensions":                     true,
		"disable-features":                       "site-per-process,TranslateUI,BlinkGenPropertyTrees",
		"disable-hang-monitor":                   true,
		"disable-ipc-flooding-protection":        true,
		"disable-popup-blocking":                 true,
		"disable-prompt-on-repost":               true,
		"disable-renderer-backgrounding":         true,
		"disable-sync":                           true,
		"force-color-profile":                    "srgb",
		"metrics-recording-only":                 true,
		"safebrowsing-disable-auto-update":       true,
		"password-store":                         "basic",
		"use-mock-keychain":                      true,
		// custom args
		"kiosk":             true,
		"enable-automation": false,
		"autoplay-policy":   "no-user-gesture-required",
	}
	if p.Insecure {
		flags["disable-web-security"] = true
		flags["allow-running-insecure-content"] = true
	}
	return flags
}
