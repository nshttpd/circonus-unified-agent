package openweathermap

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/circonus-labs/circonus-unified-agent/cua"
	"github.com/circonus-labs/circonus-unified-agent/internal"
	"github.com/circonus-labs/circonus-unified-agent/plugins/inputs"
)

const (
	// https://openweathermap.org/current#severalid
	// Call for several city IDs
	// The limit of locations is 20.
	owmRequestSeveralCityID int = 20

	defaultBaseURL                       = "https://api.openweathermap.org/"
	defaultResponseTimeout time.Duration = time.Second * 5
	defaultUnits           string        = "metric"
	defaultLang            string        = "en"
)

type OpenWeatherMap struct {
	client          *http.Client
	baseURL         *url.URL
	AppID           string            `toml:"app_id"`
	BaseURL         string            `toml:"base_url"`
	Units           string            `toml:"units"`
	Lang            string            `toml:"lang"`
	Fetch           []string          `toml:"fetch"`
	CityID          []string          `toml:"city_id"`
	ResponseTimeout internal.Duration `toml:"response_timeout"`
}

var sampleConfig = `
  ## OpenWeatherMap API key.
  app_id = "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"

  ## City ID's to collect weather data from.
  city_id = ["5391959"]

  ## Language of the description field. Can be one of "ar", "bg",
  ## "ca", "cz", "de", "el", "en", "fa", "fi", "fr", "gl", "hr", "hu",
  ## "it", "ja", "kr", "la", "lt", "mk", "nl", "pl", "pt", "ro", "ru",
  ## "se", "sk", "sl", "es", "tr", "ua", "vi", "zh_cn", "zh_tw"
  # lang = "en"

  ## APIs to fetch; can contain "weather" or "forecast".
  fetch = ["weather", "forecast"]

  ## OpenWeatherMap base URL
  # base_url = "https://api.openweathermap.org/"

  ## Timeout for HTTP response.
  # response_timeout = "5s"

  ## Preferred unit system for temperature and wind speed. Can be one of
  ## "metric", "imperial", or "standard".
  # units = "metric"

  ## Query interval; OpenWeatherMap updates their weather data every 10
  ## minutes.
  interval = "10m"
`

func (n *OpenWeatherMap) SampleConfig() string {
	return sampleConfig
}

func (n *OpenWeatherMap) Description() string {
	return "Read current weather and forecasts data from openweathermap.org"
}

func (n *OpenWeatherMap) Gather(ctx context.Context, acc cua.Accumulator) error {
	var wg sync.WaitGroup
	var strs []string

	for _, fetch := range n.Fetch {
		if fetch == "forecast" {
			for _, city := range n.CityID {
				addr := n.formatURL("/data/2.5/forecast", city)
				wg.Add(1)
				go func() {
					defer wg.Done()
					status, err := n.gatherURL(addr)
					if err != nil {
						acc.AddError(err)
						return
					}

					gatherForecast(acc, status)
				}()
			}
		} else if fetch == "weather" {
			j := 0
			for j < len(n.CityID) {
				strs = make([]string, 0)
				for i := 0; j < len(n.CityID) && i < owmRequestSeveralCityID; i++ {
					strs = append(strs, n.CityID[j])
					j++
				}
				cities := strings.Join(strs, ",")

				addr := n.formatURL("/data/2.5/group", cities)
				wg.Add(1)
				go func() {
					defer wg.Done()
					status, err := n.gatherURL(addr)
					if err != nil {
						acc.AddError(err)
						return
					}

					gatherWeather(acc, status)
				}()
			}

		}
	}

	wg.Wait()
	return nil
}

func (n *OpenWeatherMap) createHTTPClient() *http.Client {
	if n.ResponseTimeout.Duration < time.Second {
		n.ResponseTimeout.Duration = defaultResponseTimeout
	}

	client := &http.Client{
		Transport: &http.Transport{},
		Timeout:   n.ResponseTimeout.Duration,
	}

	return client
}

func (n *OpenWeatherMap) gatherURL(addr string) (*Status, error) {
	resp, err := n.client.Get(addr)
	if err != nil {
		return nil, fmt.Errorf("error making HTTP request to %s: %w", addr, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned HTTP status %s", addr, resp.Status)
	}

	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, fmt.Errorf("parse media type (%s): %w", resp.Header.Get("Content-Type"), err)
	}

	if mediaType != "application/json" {
		return nil, fmt.Errorf("%s returned unexpected content type %s", addr, mediaType)
	}

	return gatherWeatherURL(resp.Body)
}

type WeatherEntry struct {
	Name    string `json:"name"`
	Weather []struct {
		Main        string `json:"main"`
		Description string `json:"description"`
		Icon        string `json:"icon"`
		ID          int64  `json:"id"`
	} `json:"weather"`
	Sys struct {
		Country string `json:"country"`
		Sunrise int64  `json:"sunrise"`
		Sunset  int64  `json:"sunset"`
	} `json:"sys"`
	Main struct {
		Humidity int64   `json:"humidity"`
		Pressure float64 `json:"pressure"`
		Temp     float64 `json:"temp"`
	} `json:"main"`
	Rain struct {
		Rain1 float64 `json:"1h"`
		Rain3 float64 `json:"3h"`
	} `json:"rain"`
	Wind struct {
		Deg   float64 `json:"deg"`
		Speed float64 `json:"speed"`
	} `json:"wind"`
	Coord struct {
		Lat float64 `json:"lat"`
		Lon float64 `json:"lon"`
	} `json:"coord"`
	Dt     int64 `json:"dt"`
	ID     int64 `json:"id"`
	Clouds struct {
		All int64 `json:"all"`
	} `json:"clouds"`
	Visibility int64 `json:"visibility"`
}

type Status struct {
	List []WeatherEntry `json:"list"`
	City struct {
		Country string `json:"country"`
		Name    string `json:"name"`
		Coord   struct {
			Lat float64 `json:"lat"`
			Lon float64 `json:"lon"`
		} `json:"coord"`
		ID int64 `json:"id"`
	} `json:"city"`
}

func gatherWeatherURL(r io.Reader) (*Status, error) {
	dec := json.NewDecoder(r)
	status := &Status{}
	if err := dec.Decode(status); err != nil {
		return nil, fmt.Errorf("error while decoding JSON response: %w", err)
	}
	return status, nil
}

func gatherRain(e WeatherEntry) float64 {
	if e.Rain.Rain1 > 0 {
		return e.Rain.Rain1
	}
	return e.Rain.Rain3
}

func gatherWeather(acc cua.Accumulator, status *Status) {
	for _, e := range status.List {
		tm := time.Unix(e.Dt, 0)

		fields := map[string]interface{}{
			"cloudiness":   e.Clouds.All,
			"humidity":     e.Main.Humidity,
			"pressure":     e.Main.Pressure,
			"rain":         gatherRain(e),
			"sunrise":      time.Unix(e.Sys.Sunrise, 0).UnixNano(),
			"sunset":       time.Unix(e.Sys.Sunset, 0).UnixNano(),
			"temperature":  e.Main.Temp,
			"visibility":   e.Visibility,
			"wind_degrees": e.Wind.Deg,
			"wind_speed":   e.Wind.Speed,
		}
		tags := map[string]string{
			"city":     e.Name,
			"city_id":  strconv.FormatInt(e.ID, 10),
			"country":  e.Sys.Country,
			"forecast": "*",
		}

		if len(e.Weather) > 0 {
			fields["condition_description"] = e.Weather[0].Description
			fields["condition_icon"] = e.Weather[0].Icon
			tags["condition_id"] = strconv.FormatInt(e.Weather[0].ID, 10)
			tags["condition_main"] = e.Weather[0].Main
		}

		acc.AddFields("weather", fields, tags, tm)
	}
}

func gatherForecast(acc cua.Accumulator, status *Status) {
	tags := map[string]string{
		"city_id":  strconv.FormatInt(status.City.ID, 10),
		"forecast": "*",
		"city":     status.City.Name,
		"country":  status.City.Country,
	}
	for i, e := range status.List {
		tm := time.Unix(e.Dt, 0)
		fields := map[string]interface{}{
			"cloudiness":   e.Clouds.All,
			"humidity":     e.Main.Humidity,
			"pressure":     e.Main.Pressure,
			"rain":         gatherRain(e),
			"temperature":  e.Main.Temp,
			"wind_degrees": e.Wind.Deg,
			"wind_speed":   e.Wind.Speed,
		}
		if len(e.Weather) > 0 {
			fields["condition_description"] = e.Weather[0].Description
			fields["condition_icon"] = e.Weather[0].Icon
			tags["condition_id"] = strconv.FormatInt(e.Weather[0].ID, 10)
			tags["condition_main"] = e.Weather[0].Main
		}
		tags["forecast"] = fmt.Sprintf("%dh", (i+1)*3)
		acc.AddFields("weather", fields, tags, tm)
	}
}

func init() {
	inputs.Add("openweathermap", func() cua.Input {
		tmout := internal.Duration{
			Duration: defaultResponseTimeout,
		}
		return &OpenWeatherMap{
			ResponseTimeout: tmout,
			BaseURL:         defaultBaseURL,
		}
	})
}

func (n *OpenWeatherMap) Init() error {
	var err error
	n.baseURL, err = url.Parse(n.BaseURL)
	if err != nil {
		return fmt.Errorf("url parse (%s): %w", n.BaseURL, err)
	}

	// Create an HTTP client that is re-used for each
	// collection interval
	n.client = n.createHTTPClient()

	switch n.Units {
	case "imperial", "standard", "metric":
	case "":
		n.Units = defaultUnits
	default:
		return fmt.Errorf("unknown units: %s", n.Units)
	}

	switch n.Lang {
	case "ar", "bg", "ca", "cz", "de", "el", "en", "fa", "fi", "fr", "gl",
		"hr", "hu", "it", "ja", "kr", "la", "lt", "mk", "nl", "pl",
		"pt", "ro", "ru", "se", "sk", "sl", "es", "tr", "ua", "vi",
		"zh_cn", "zh_tw":
	case "":
		n.Lang = defaultLang
	default:
		return fmt.Errorf("unknown language: %s", n.Lang)
	}

	return nil
}

func (n *OpenWeatherMap) formatURL(path string, city string) string {
	v := url.Values{
		"id":    []string{city},
		"APPID": []string{n.AppID},
		"lang":  []string{n.Lang},
		"units": []string{n.Units},
	}

	relative := &url.URL{
		Path:     path,
		RawQuery: v.Encode(),
	}

	return n.baseURL.ResolveReference(relative).String()
}
