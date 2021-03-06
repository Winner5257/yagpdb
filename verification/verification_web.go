package verification

import (
	"database/sql"
	"encoding/json"
	"github.com/jonas747/yagpdb/common"
	"github.com/jonas747/yagpdb/common/scheduledevents2"
	"github.com/jonas747/yagpdb/verification/models"
	"github.com/jonas747/yagpdb/web"
	"github.com/microcosm-cc/bluemonday"
	"github.com/russross/blackfriday"
	"github.com/volatiletech/null"
	"github.com/volatiletech/sqlboiler/boil"
	"goji.io/pat"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type FormData struct {
	Enabled             bool
	VerifiedRole        int64  `valid:"role"`
	PageContent         string `valid:",10000"`
	KickUnverifiedAfter int
	WarnUnverifiedAfter int
	WarnMessage         string `valid:"template,10000"`
	DMMessage           string `valid:"template,10000"`
	LogChannel          int64  `valid:"channel,true"`
}

func (p *Plugin) InitWeb() {
	web.LoadHTMLTemplate("../../verification/assets/verification_control_panel.html", "templates/plugins/verification_control_panel.html")
	web.LoadHTMLTemplate("../../verification/assets/verification_verify_page.html", "templates/plugins/verification_verify_page.html")

	getHandler := web.ControllerHandler(p.handleGetSettings, "cp_verification_settings")
	postHandler := web.ControllerPostHandler(p.handlePostSettings, getHandler, FormData{}, "Updated verification settings")

	web.CPMux.Handle(pat.Get("/verification"), web.RequireBotMemberMW(web.RequireGuildChannelsMiddleware(getHandler)))
	web.CPMux.Handle(pat.Get("/verification/"), web.RequireBotMemberMW(web.RequireGuildChannelsMiddleware(getHandler)))

	web.CPMux.Handle(pat.Post("/verification"), web.RequireGuildChannelsMiddleware(postHandler))

	getVerifyPageHandler := web.ControllerHandler(p.handleGetVerifyPage, "verification_verify_page")
	postVerifyPageHandler := web.ControllerPostHandler(p.handlePostVerifyPage, getVerifyPageHandler, nil, "verification_verify_page")
	web.ServerPublicMux.Handle(pat.Get("/verify/:user_id/:token"), getVerifyPageHandler)
	web.ServerPublicMux.Handle(pat.Post("/verify/:user_id/:token"), postVerifyPageHandler)
}

func (p *Plugin) handleGetSettings(w http.ResponseWriter, r *http.Request) (web.TemplateData, error) {
	ctx := r.Context()
	g, templateData := web.GetBaseCPContextData(ctx)

	settings, err := models.FindVerificationConfigG(ctx, g.ID)
	if err == sql.ErrNoRows {
		settings = &models.VerificationConfig{
			GuildID: g.ID,
		}
		err = nil
	}

	if settings != nil && settings.DMMessage == "" {
		settings.DMMessage = DefaultDMMessage
	}

	templateData["DefaultPageContent"] = DefaultPageContent
	templateData["PluginSettings"] = settings

	return templateData, err
}

func (p *Plugin) handlePostSettings(w http.ResponseWriter, r *http.Request) (web.TemplateData, error) {
	ctx := r.Context()
	g, templateData := web.GetBaseCPContextData(ctx)

	formConfig := ctx.Value(common.ContextKeyParsedForm).(*FormData)

	model := &models.VerificationConfig{
		GuildID:             g.ID,
		Enabled:             formConfig.Enabled,
		VerifiedRole:        formConfig.VerifiedRole,
		PageContent:         formConfig.PageContent,
		KickUnverifiedAfter: formConfig.KickUnverifiedAfter,
		WarnUnverifiedAfter: formConfig.WarnUnverifiedAfter,
		WarnMessage:         formConfig.WarnMessage,
		LogChannel:          formConfig.LogChannel,
		DMMessage:           formConfig.DMMessage,
	}

	columns := boil.Whitelist("enabled", "verified_role", "page_content", "kick_unverified_after", "warn_unverified_after", "warn_message", "log_channel", "dm_message")
	columnsCreate := boil.Whitelist("guild_id", "enabled", "verified_role", "page_content", "kick_unverified_after", "warn_unverified_after", "warn_message", "log_channel", "dm_message")
	err := model.UpsertG(ctx, true, []string{"guild_id"}, columns, columnsCreate)
	return templateData, err
}

func (p *Plugin) handleGetVerifyPage(w http.ResponseWriter, r *http.Request) (web.TemplateData, error) {
	ctx := r.Context()
	g, templateData := web.GetBaseCPContextData(ctx)

	// render main page content
	settings, err := models.FindVerificationConfigG(ctx, g.ID)
	if err == sql.ErrNoRows {
		settings = &models.VerificationConfig{
			GuildID: g.ID,
		}
		err = nil
	}

	if err != nil {
		return templateData, err
	}

	if !settings.Enabled {
		templateData.AddAlerts(web.ErrorAlert("Verification system disabled on this server"))
		return templateData, nil
	}

	if _, ok := templateData["REValid"]; !ok {
		// check if there's a valid session if we didn't just finish verifying
		userID, _ := strconv.ParseInt(pat.Param(r, "user_id"), 10, 64)
		token := pat.Param(r, "token")
		_, err = models.VerificationSessions(
			models.VerificationSessionWhere.UserID.EQ(userID),
			models.VerificationSessionWhere.Token.EQ(token),
			models.VerificationSessionWhere.ExpiredAt.IsNull(),
			models.VerificationSessionWhere.SolvedAt.IsNull()).OneG(ctx)

		if err != nil {
			if err == sql.ErrNoRows {
				templateData.AddAlerts(web.ErrorAlert("No verification session, try rejoining the server or contact an admin if the problem persist"))
				return templateData, nil
			}

			return templateData, err
		}
	}

	templateData["ExtraHead"] = template.HTML(`<script src="https://www.google.com/recaptcha/api.js" async defer></script>`)
	templateData["GoogleReCaptchaSiteKey"] = GoogleReCAPTCHASiteKey

	msg := settings.PageContent
	if msg == "" {
		msg = DefaultPageContent
	}

	unsafe := blackfriday.MarkdownCommon([]byte(msg))
	html := bluemonday.UGCPolicy().SanitizeBytes(unsafe)
	templateData["RenderedPageContent"] = template.HTML(html)

	return templateData, nil
}

func (p *Plugin) handlePostVerifyPage(w http.ResponseWriter, r *http.Request) (web.TemplateData, error) {
	ctx := r.Context()
	g, templateData := web.GetBaseCPContextData(ctx)

	settings, err := models.FindVerificationConfigG(ctx, g.ID)
	if err == sql.ErrNoRows {
		settings = &models.VerificationConfig{
			GuildID: g.ID,
		}
		err = nil
	}

	if err != nil {
		return templateData, err
	}

	if !settings.Enabled {
		templateData.AddAlerts(web.ErrorAlert("Verification system disabled on this server"))
		return templateData, nil
	}

	valid, err := p.checkCAPTCHAResponse(r.FormValue("g-recaptcha-response"))

	token := pat.Param(r, "token")
	userID, _ := strconv.ParseInt(pat.Param(r, "user_id"), 10, 64)

	verSession, err := models.VerificationSessions(
		models.VerificationSessionWhere.UserID.EQ(userID),
		models.VerificationSessionWhere.Token.EQ(token),
		models.VerificationSessionWhere.ExpiredAt.IsNull(),
		models.VerificationSessionWhere.SolvedAt.IsNull()).OneG(ctx)

	if err != nil {
		if err == sql.ErrNoRows {
			templateData.AddAlerts(web.ErrorAlert("No verification session, try rejoining the server or contact an admin if the problem persist"))
			return templateData, nil
		}

		return templateData, err
	}

	if valid {
		scheduledevents2.ScheduleEvent("verification_user_verified", g.ID, time.Now(), userID)
		verSession.SolvedAt = null.TimeFrom(time.Now())
		verSession.UpdateG(ctx, boil.Infer())
	} else {
		templateData.AddAlerts(web.ErrorAlert("Invalid reCAPTCHA submission."))
	}

	templateData["REValid"] = valid

	return templateData, err
}

type CheckCAPTCHAResponse struct {
	Success     bool     `json:"success"`
	ChallengeTS string   `json:"challenge_ts"`
	Hostname    string   `json:"hostname"`
	ErrorCodes  []string `json:"error-codes"`
}

type CheckCAPTCHARequest struct {
	Secret   string `json:"secret"`
	Response string `json:"response"`
}

func (p *Plugin) checkCAPTCHAResponse(response string) (valid bool, err error) {

	v := url.Values{
		"response": {response},
		"secret":   {GoogleReCAPTCHASecret},
	}

	resp, err := http.PostForm("https://www.google.com/recaptcha/api/siteverify", v)
	if err != nil {
		return false, err
	}

	defer resp.Body.Close()

	var dst CheckCAPTCHAResponse
	decoder := json.NewDecoder(resp.Body)
	err = decoder.Decode(&dst)
	if err != nil {
		return false, err
	}

	if !dst.Success {
		logger.Warnf("reCAPTCHA failed: %#v", dst)
	}

	return dst.Success, nil
}
