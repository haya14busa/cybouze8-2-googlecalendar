package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/transform"
	"google.golang.org/api/calendar/v3"

	"github.com/PuerkitoBio/goquery"
)

var (
	cybozuUserID string
	cybozuUserPW string
	baseURL      string // http://example/cgi-bin/cbag/ag.cgi

	calendarId string // xxxxxxxxxxxxxxxxxxxxxxxxxx@group.calendar.google.com
	userID     string
)

func initConfig() {
	cybozuUserID = getConfig("C2G_CYBOZU_USERID")
	cybozuUserPW = getConfig("C2G_CYBOZU_USERPW")
	baseURL = getConfig("C2G_CYBOZU_BASE_URL")

	calendarId = getConfig("C2G_CALENDAR_ID")
	userID = cybozuUserID // TODO: support other users???
}

func getConfig(key string) string {
	r := os.Getenv(key)
	if r == "" {
		log.Fatalf("Environment variable not set: %s", key)
	}
	return r
}

func main() {
	log.Println("===Start: cybozu8togcal===")

	initConfig()

	gcal := getGcal()

	agsessid, err := getAGSESSID()
	if err != nil {
		log.Fatalf("Cannot get AGSESSID from cybozu", err)
	}

	node := calendarHtml(agsessid, cybozuUserID, userID)

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

	doc.Find(".bannerevent").Each(func(i int, s *goquery.Selection) {
		waitGroup.Add(1)
		go func(s *goquery.Selection) {
			defer waitGroup.Done()
			updateBannerEvent(gcal, s, agsessid)
		}(s)
	})
	waitGroup.Wait()

	log.Println("===Finish: cybozu8togcal===")
}

func getGcal() *GoogleCalendar {
	ctx := context.Background()

	b, err := ioutil.ReadFile(configFilePath("client_secret.json"))
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
	tommorow := time.Now().AddDate(0, 0, 1)
	if err != nil {
		return err
	}
	var waitGroup sync.WaitGroup
	for _, item := range allEvents.Items {
		waitGroup.Add(1)
		go func(item *calendar.Event) {
			defer waitGroup.Done()
			startTime, err := time.Parse(time.RFC3339, item.Start.DateTime)
			if err == nil && startTime.Before(tommorow) {
				return
			}
			startDate, err := time.Parse("2006-01-02", item.Start.DateTime)
			if err == nil && startDate.Before(tommorow) {
				return
			}
			if err := this.DeleteEvent(item); err != nil {
				log.Printf("Unable to delete event: %v", err)
			} else {
				log.Printf("delete upcoming event: %v", item.Summary)
			}
		}(item)
	}
	waitGroup.Wait()
	return nil
}

func (this *GoogleCalendar) DeleteEvent(event *calendar.Event) error {
	err := this.svc.Events.Delete(this.calendarId, event.Id).Do()
	if err != nil {
		rateLimitExeeded, _ := regexp.MatchString("403: Rate Limit Exceeded", err.Error())
		if rateLimitExeeded {
			log.Printf("Unable to delete event '%v'. retry after 10 seconds: %v", event.Summary, err)
			time.Sleep(10 * time.Second)
			return this.DeleteEvent(event)
		}
	}
	return err
}

func updateBannerEvent(gcal *GoogleCalendar, s *goquery.Selection, agsessid string) {
	href, _ := s.Attr("href")
	queryParamRe := regexp.MustCompile(`\?.*$`)
	queryParam := queryParamRe.FindString(href)

	url := baseURL + strings.Replace(queryParam, "?page=ScheduleView", "?page=ScheduleBannerModify", 1)
	node, err := cybozuHtml(agsessid, cybozuUserID, userID, url)
	if err != nil {
		log.Printf("fail to get html node: %v", err)
		return
	}
	doc := goquery.NewDocumentFromNode(node)
	startYear, err := selectedIntValue(doc, "SetDate.Year")
	startMonth, err := selectedIntValue(doc, "SetDate.Month")
	startDay, err := selectedIntValue(doc, "SetDate.Day")
	endYear, err := selectedIntValue(doc, "EndDate.Year")
	endMonth, err := selectedIntValue(doc, "EndDate.Month")
	endDay, err := selectedIntValue(doc, "EndDate.Day")
	if err != nil {
		log.Printf("Cannot parse event date: %v", err)
		return
	}

	title, _ := s.Attr("title")

	loc, _ := time.LoadLocation("Asia/Tokyo")
	startDateTime := time.Date(startYear, time.Month(startMonth), startDay, 0, 0, 0, 0, loc)
	endDateTime := time.Date(endYear, time.Month(endMonth), endDay, 0, 0, 0, 0, loc)
	startDate := startDateTime.Format("2006-01-02")
	endDate := endDateTime.Format("2006-01-02")

	eventIdRe := regexp.MustCompile(`sEID=([\d]+)`)
	eventId := eventIdRe.FindStringSubmatch(href)[1] + fmt.Sprintf("%d%d%d", startYear, startMonth, startDay)

	event := &calendar.Event{
		Id:      eventId,
		Summary: title,
		Start: &calendar.EventDateTime{
			Date:     startDate,
			TimeZone: "Asia/Tokyo",
		},
		End: &calendar.EventDateTime{
			Date:     endDate,
			TimeZone: "Asia/Tokyo",
		},
	}

	if _, err := gcal.Upsert(event); err != nil {
		log.Printf("Unable to upsert bannerevent '%v': %v", event.Summary, err)
	} else {
		log.Printf("Succeed to update bannerevent %v-%v, %v", startDate, endDate, title)
	}
}

func selectedIntValue(doc *goquery.Document, name string) (int, error) {
	re := regexp.MustCompile(`^\d+`)
	var r int
	found := false
	doc.Find(fmt.Sprintf("select[name='%s'] option", name)).Each(func(i int, s *goquery.Selection) {
		if _, ok := s.Attr("selected"); ok {
			found = true
			r, _ = strconv.Atoi(re.FindString(s.Text()))
		}
	})
	if found {
		return r, nil
	}
	return 0, fmt.Errorf("selected value doesn't exist for '%s'", name)
}

func updateEvent(gcal *GoogleCalendar, s *goquery.Selection) {
	href, _ := s.Attr("href")
	re := regexp.MustCompile("Date=da\\.(?P<year>[\\d]{4})\\.(?P<month>[\\d]{1,2})\\.(?P<day>[\\d]{1,2})")
	matches := re.FindStringSubmatch(href)
	year, err := strconv.Atoi(matches[1])
	month, err := strconv.Atoi(matches[2])
	day, err := strconv.Atoi(matches[3])
	if err != nil {
		log.Printf("Cannot parse event date: %v", err)
		return
	}

	loc, _ := time.LoadLocation("Asia/Tokyo")

	dateTime := time.Date(year, time.Month(month), day, 0, 0, 0, 0, loc)
	if dateTime.Before(time.Now().AddDate(0, 0, -7)) {
		// Update upcoming events, not old events
		return
	}
	date := dateTime.Format("2006-01-02")

	eventIdRe := regexp.MustCompile(`sEID=([\d]+)`)
	eventId := eventIdRe.FindStringSubmatch(href)[1] + fmt.Sprintf("%d%d%d", year, month, day)
	title := s.Find(".eventTitle").Text()

	eventTimeRe := regexp.MustCompile(`^(\d{2}):(\d{2})(?:-(\d{2}):(\d{2}))?`)
	eventTime := eventTimeRe.FindStringSubmatch(title)

	// Remove time parts from title
	title = eventTimeRe.ReplaceAllString(title, "")

	event := &calendar.Event{
		Id:      eventId,
		Summary: title,
	}

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
			"_ID":         {cybozuUserID},
			"Password":    {cybozuUserPW},
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
	node, err := cybozuHtml(agsessid, loginid, userID, url)
	if err != nil {
		panic(err)
	}
	return node
}

func cybozuHtml(agsessid, loginid, userID, url string) (*html.Node, error) {
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
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	buf := new(bytes.Buffer)
	buf.ReadFrom(resp.Body)
	body := buf.String()

	utf8, err := convertShiftJIS2utf8(strings.NewReader(body))
	if err != nil {
		return nil, err
	}

	node, err := html.Parse(strings.NewReader(utf8))
	if err != nil {
		return nil, err
	}
	return node, nil
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
	return configFilePath("token.json"), nil
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

func configFilePath(filename string) string {
	usr, err := user.Current()
	if err != nil {
		return ""
	}
	configDir := filepath.Join(usr.HomeDir, ".config", "cybozu8-2-googlecalendar")
	os.MkdirAll(configDir, 0700)
	return filepath.Join(configDir, url.QueryEscape(filename))
}

func convertShiftJIS2utf8(inStream io.Reader) (string, error) {
	//read from stream (Shift-JIS to UTF-8)
	scanner := bufio.NewScanner(transform.NewReader(inStream, japanese.ShiftJIS.NewDecoder()))
	list := make([]string, 0)
	for scanner.Scan() {
		list = append(list, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return strings.Join(list, ""), nil
}
