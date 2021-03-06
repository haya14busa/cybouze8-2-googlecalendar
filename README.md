:date: cybozu-2-googlecalendar :date:
=======================================

Sync calendar of Cybozu Office Version 8.0.1 with Google Calendar

How to Use
----------
1. `$ go get -u github.com/haya14busa/cybozu-2-googlecalendar`
2. Create a project and `Create credentials` with OAuth client ID whose `Application type` is `Other` https://console.developers.google.com/project
3. Download and put `client_secret.json` to `~/.config/cybozu-2-googlecalendar/client_secret.json`.
4. Create Google Calendar to sync with and get calendar id (`xxxxxxxxxxxxxxxxxxxxxxxxxx@group.calendar.google.com`). You can find calendar id from Calendar setting page
5. Configure required environment variables
6. `$ cybozu-2-googlecalendar`

### Environment variables

```sh
export C2G_CYBOZU_USERID="<user ID of cybozu. you can find it from `UID` query parameter of Cybozu page url>"
export C2G_CYBOZU_USERPW="<password of cybozu>"
export C2G_CYBOZU_BASE_URL="<base url of cybozu like http://example.com/cgi-bin/cbag/ag.cgi>"
export C2G_CALENDAR_ID="<google calendar id like xxxxxxxxxxxxxxxxxxxxxxxxxx@group.calendar.google.com>"
```
