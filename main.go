package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/context"
	"golang.org/x/net/html"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/calendar/v3"

	"github.com/PuerkitoBio/goquery"
	"github.com/djimenez/iconv-go"
)

var (
	cybozeUserID string
	cybozeUserPW string
	baseURL      string // http://example/cgi-bin/cbag/ag.cgi

	calendarId string // xxxxxxxxxxxxxxxxxxxxxxxxxx@group.calendar.google.com
	userID     string
)

func initConfig() {
	cybozeUserID = getConfig("C2G_CYBOZE_USERID")
	cybozeUserPW = getConfig("C2G_CYBOZE_USERPW")
	baseURL = getConfig("C2G_BASE_URL")

	calendarId = getConfig("C2G_CALENDAR_ID")
	userID = cybozeUserID // TODO: support other users???
}

func getConfig(key string) string {
	r := os.Getenv(key)
	if r == "" {
		log.Fatalf("Environment variable not set: %s", key)
	}
	return r
}

func main() {
	log.Println("===Start: cybouze8togcal===")

	initConfig()

	gcal := getGcal()

	agsessid, err := getAGSESSID()
	if err != nil {
		log.Fatalf("Cannot get AGSESSID from cyboze", err)
	}

	node := calendarHtml(agsessid, cybozeUserID, userID)
	doc := goquery.NewDocumentFromNode(node)

	gcal.DeleteUpcomingEvents()

	var waitGroup sync.WaitGroup
	doc.Find(".event").Each(func(i int, s *goquery.Selection) {
		waitGroup.Add(1)
		go func(s *goquery.Selection) {
			defer waitGroup.Done()
			updateEvent(gcal, s)
		}(s)
	})
	waitGroup.Wait()

	log.Println("===Finish: cybouze8togcal===")
}

func getGcal() *GoogleCalendar {
	ctx := context.Background()

	b, err := ioutil.ReadFile("client_secret.json")
	if err != nil {
		log.Fatalf("Unable to read client secret file: %v", err)
	}

	config, err := google.ConfigFromJSON(b, calendar.CalendarScope)
	if err != nil {
		log.Fatalf("Unable to parse client secret file to config: %v", err)
	}

	client := getClient(ctx, config)
	return NewGoogleCalendar(client, calendarId)
}

type GoogleCalendar struct {
	calendarId string
	svc        *calendar.Service
}

func NewGoogleCalendar(client *http.Client, calendarId string) *GoogleCalendar {
	this := &GoogleCalendar{}
	this.svc, _ = calendar.New(client)
	this.calendarId = calendarId
	return this
}

func (this *GoogleCalendar) Upsert(event *calendar.Event) (*calendar.Event, error) {
	ret, err := this.svc.Events.Update(this.calendarId, event.Id, event).Do()
	if err != nil {
		ret, err = this.svc.Events.Insert(this.calendarId, event).Do()
		if err != nil {
			rateLimitExeeded, _ := regexp.MatchString("403: Rate Limit Exceeded", err.Error())
			if rateLimitExeeded {
				log.Printf("Unable to upsert event '%v'. retry after 10 seconds: %v", event.Summary, err)
				time.Sleep(10 * time.Second)
				return this.Upsert(event)
			}
			return nil, err
		}
	}
	return ret, nil
}

func (this *GoogleCalendar) DeleteUpcomingEvents() error {
	allEvents, err := this.svc.Events.List(calendarId).Do()
	now := time.Now()
	if err != nil {
		return err
	}
	var waitGroup sync.WaitGroup
	for _, item := range allEvents.Items {
		waitGroup.Add(1)
		go func(item *calendar.Event) {
			defer waitGroup.Done()
			startTime, err := time.Parse(time.RFC3339, item.Start.DateTime)
			if err == nil && startTime.Before(now) {
				return
			}
			startDate, err := time.Parse("2006-01-02", item.Start.DateTime)
			if err == nil && startDate.Before(now) {
				return
			}
			log.Printf("delete upcoming event: %v", item.Summary)
			if err := this.svc.Events.Delete(this.calendarId, item.Id).Do(); err != nil {
				log.Printf("Unable to delete event: %v", err)
			}
		}(item)
	}
	waitGroup.Wait()
	return nil
}

func updateEvent(gcal *GoogleCalendar, s *goquery.Selection) {
	href, _ := s.Attr("href")
	re := regexp.MustCompile("Date=da\\.(?P<year>[\\d]{4})\\.(?P<month>[\\d]{1,2})\\.(?P<day>[\\d]{1,2})")
	matches := re.FindStringSubmatch(href)
	year, err := strconv.Atoi(matches[1])
	month, err := strconv.Atoi(matches[2])
	day, err := strconv.Atoi(re.FindStringSubmatch(href)[3])
	if err != nil {
		log.Printf("Cannot parse event date: %v", err)
		return
	}

	loc, _ := time.LoadLocation("Asia/Tokyo")

	dateTime := time.Date(year, time.Month(month), day, 0, 0, 0, 0, loc)
	if dateTime.Before(time.Now()) {
		// Update upcoming events, not old events
		return
	}
	date := dateTime.Format("2006-01-02")

	eventIdRe := regexp.MustCompile(`sEID=([\d]+)`)
	eventId := eventIdRe.FindStringSubmatch(href)[1] + fmt.Sprintf("%d%d%d", year, month, day)
	title := s.Find(".eventTitle").Text()

	event := &calendar.Event{
		Id:      eventId,
		Summary: title,
	}

	eventTimeRe := regexp.MustCompile(`^(\d{2}):(\d{2})(?:-(\d{2}):(\d{2}))?`)
	eventTime := eventTimeRe.FindStringSubmatch(title)

	if len(eventTime) == 5 { // Match!
		startHour, _ := strconv.Atoi(eventTime[1])
		startMin, _ := strconv.Atoi(eventTime[2])
		startDate := time.Date(year, time.Month(month), day, startHour, startMin, 0, 0, loc).Format(time.RFC3339)
		endHour, err := strconv.Atoi(eventTime[3])
		if err != nil {
			endHour = startHour + 1
		}
		endMin, err := strconv.Atoi(eventTime[4])
		if err != nil {
			endMin = startMin
		}
		endDate := time.Date(year, time.Month(month), day, endHour, endMin, 0, 0, loc).Format(time.RFC3339)
		event.Start = &calendar.EventDateTime{
			DateTime: startDate,
			TimeZone: "Asia/Tokyo",
		}
		event.End = &calendar.EventDateTime{
			DateTime: endDate,
			TimeZone: "Asia/Tokyo",
		}
	} else {
		event.Start = &calendar.EventDateTime{
			Date:     date,
			TimeZone: "Asia/Tokyo",
		}
		event.End = &calendar.EventDateTime{
			Date:     date,
			TimeZone: "Asia/Tokyo",
		}
	}

	if _, err := gcal.Upsert(event); err != nil {
		log.Printf("Unable to upsert event '%v': %v", event.Summary, err)
	} else {
		log.Printf("Succeed to update event %v, %v", date, title)
	}
}

func getAGSESSID() (string, error) {
	resp, err := http.PostForm(baseURL,
		url.Values{
			"_ID":         {cybozeUserID},
			"Password":    {cybozeUserPW},
			"csrf_ticket": {""},
			"_System":     {"login"},
			"_Login":      {"1"},
			"LoginMethod": {"0"},
		})
	if err != nil {
		return "", err
	}
	header := resp.Header
	cookies := strings.Split(header.Get("Set-Cookie"), ";")
	for _, cookie := range cookies {
		pair := strings.SplitN(cookie, "=", 2)
		k, v := pair[0], pair[1]
		if k == "AGSESSID" {
			return v, nil
		}
	}
	return "", errors.New("cannot get AGSESSID")
}

func calendarHtml(agsessid, loginid, userID string) *html.Node {
	now := time.Now().Format("2006.01.02")
	url := fmt.Sprintf("%s?page=ScheduleUserMonth&UID=%s&Date=da.%s", baseURL, userID, now)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Accept-Encoding", "gzip, deflate, sdch")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/49.0.2623.75 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Cookie", fmt.Sprintf("AGSESSID=%s; AGLOGINID=%s;", agsessid, loginid))
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Cache-Control", "no-cache")

	client := new(http.Client)
	resp, _ := client.Do(req)
	defer resp.Body.Close()
	utfBody, _ := iconv.NewReader(resp.Body, "Shift_JIS", "utf-8")
	node, _ := html.Parse(utfBody)
	return node
}

// getClient uses a Context and Config to retrieve a Token
// then generate a Client. It returns the generated Client.
func getClient(ctx context.Context, config *oauth2.Config) *http.Client {
	cacheFile, err := tokenCacheFile()
	if err != nil {
		log.Fatalf("Unable to get path to cached credential file. %v", err)
	}
	tok, err := tokenFromFile(cacheFile)
	if err != nil {
		tok = getTokenFromWeb(config)
		saveToken(cacheFile, tok)
	}
	return config.Client(ctx, tok)
}

// getTokenFromWeb uses Config to request a Token.
// It returns the retrieved Token.
func getTokenFromWeb(config *oauth2.Config) *oauth2.Token {
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	fmt.Printf("Go to the following link in your browser then type the "+
		"authorization code: \n%v\n", authURL)

	var code string
	if _, err := fmt.Scan(&code); err != nil {
		log.Fatalf("Unable to read authorization code %v", err)
	}

	tok, err := config.Exchange(oauth2.NoContext, code)
	if err != nil {
		log.Fatalf("Unable to retrieve token from web %v", err)
	}
	return tok
}

// tokenCacheFile generates credential file path/filename.
// It returns the generated credential path/filename.
func tokenCacheFile() (string, error) {
	usr, err := user.Current()
	if err != nil {
		return "", err
	}
	tokenCacheDir := filepath.Join(usr.HomeDir, ".credentials")
	os.MkdirAll(tokenCacheDir, 0700)
	return filepath.Join(tokenCacheDir,
		url.QueryEscape("cybouze8togcal.json")), err
}

// tokenFromFile retrieves a Token from a given file path.
// It returns the retrieved Token and any read error encountered.
func tokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	t := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(t)
	defer f.Close()
	return t, err
}

// saveToken uses a file path to create a file and store the
// token in it.
func saveToken(file string, token *oauth2.Token) {
	fmt.Printf("Saving credential file to: %s\n", file)
	f, err := os.Create(file)
	if err != nil {
		log.Fatalf("Unable to cache oauth token: %v", err)
	}
	defer f.Close()
	json.NewEncoder(f).Encode(token)
}
