package main

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coolapso/c4stack/accounts"
	"github.com/coolapso/c4stack/apikeys"
	"github.com/coolapso/c4stack/credits"
	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
)

func (t *textlab) home(c *gin.Context) {
	t.layout.Render(c, http.StatusOK, "", homePage())
}

// job is a row of the app's jobs table.
type job struct {
	ID            string
	Input         string
	ReservedCents int64
	ChargedCents  sql.NullInt64
	Status        string
	CreatedAt     time.Time
}

func (t *textlab) recentJobs(c *gin.Context, limit int) ([]job, error) {
	rows, err := t.db.QueryContext(c.Request.Context(), `
		SELECT id, input, reserved_cents, charged_cents, status, created_at
		FROM jobs
		WHERE account_id = $1
		ORDER BY created_at DESC
		LIMIT $2`, accounts.Current(c).ID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.ID, &j.Input, &j.ReservedCents, &j.ChargedCents, &j.Status, &j.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (t *textlab) overview(c *gin.Context) {
	summary, err := t.credits.Summary(c.Request.Context(), accounts.Current(c).ID)
	if err != nil {
		t.fail(c, err)
		return
	}
	keys, err := t.keys.List(c.Request.Context(), accounts.Current(c).ID)
	if err != nil {
		t.fail(c, err)
		return
	}
	jobs, err := t.recentJobs(c, 5)
	if err != nil {
		t.fail(c, err)
		return
	}
	active := 0
	for _, k := range keys {
		if k.RevokedAt == nil {
			active++
		}
	}
	t.layout.Render(c, http.StatusOK, "Overview", overviewPage(summary, active, jobs))
}

func (t *textlab) jobs(c *gin.Context) {
	jobs, err := t.recentJobs(c, 50)
	if err != nil {
		t.fail(c, err)
		return
	}
	t.layout.Render(c, http.StatusOK, "Jobs", jobsPage(jobs))
}

func (t *textlab) fail(c *gin.Context, err error) {
	web.Log(c).Error("page failed", "error", err)
	t.layout.Error(c, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
}

// API ---------------------------------------------------------------------------

func apiError(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": code, "message": message})
}

func (t *textlab) balance(c *gin.Context) {
	s, err := t.credits.Summary(c.Request.Context(), apikeys.Current(c).AccountID)
	if err != nil {
		web.Log(c).Error("balance failed", "error", err)
		apiError(c, http.StatusServiceUnavailable, "unavailable", "Try again in a moment.")
		return
	}
	c.JSON(http.StatusOK, gin.H{"balance_cents": s.BalanceCents, "held_cents": s.HeldCents, "available_cents": s.AvailableCents})
}

type textRequest struct {
	Text string `json:"text"`
}

func readText(c *gin.Context) (string, bool) {
	var req textRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			apiError(c, http.StatusRequestEntityTooLarge, "too_large", "Send at most 64 KB of text.")
			return "", false
		}
		apiError(c, http.StatusBadRequest, "invalid_request", `Send JSON: {"text": "..."}`)
		return "", false
	}
	if strings.TrimSpace(req.Text) == "" || !utf8.ValidString(req.Text) {
		apiError(c, http.StatusBadRequest, "invalid_request", "The text is empty or not UTF-8.")
		return "", false
	}
	return req.Text, true
}

// analyseCost is what one analysis costs, in credits (EUR cents).
const analyseCost = 2

// analyse counts words, characters and lines. The Idempotency-Key header makes a
// retried request free: the same key is charged once.
func (t *textlab) analyse(c *gin.Context) {
	key := c.GetHeader("Idempotency-Key")
	if key == "" || len(key) > 100 {
		apiError(c, http.StatusBadRequest, "idempotency_key_required", "Send an Idempotency-Key header of up to 100 characters.")
		return
	}
	text, ok := readText(c)
	if !ok {
		return
	}
	err := t.credits.Debit(c.Request.Context(), apikeys.Current(c).AccountID, analyseCost, "analyse:"+key, "Text analysis")
	switch {
	case errors.Is(err, credits.ErrInsufficient):
		apiError(c, http.StatusPaymentRequired, "insufficient_credit", "Buy credit to continue.")
		return
	case errors.Is(err, credits.ErrConflict):
		apiError(c, http.StatusConflict, "idempotency_key_reused", "This Idempotency-Key was used for another charge.")
		return
	case err != nil:
		web.Log(c).Error("analyse debit failed", "error", err)
		apiError(c, http.StatusServiceUnavailable, "unavailable", "Try again in a moment.")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"words":         len(strings.Fields(text)),
		"characters":    utf8.RuneCountInString(text),
		"lines":         strings.Count(text, "\n") + 1,
		"charged_cents": analyseCost,
	})
}

// jobCost is one credit per started 100 characters.
func jobCost(text string) int64 {
	return int64(utf8.RuneCountInString(text)+99) / 100
}

// createJob reserves the job's cost as a hold, inside the account's lock, so the
// reserved credit cannot be spent twice.
func (t *textlab) createJob(c *gin.Context) {
	text, ok := readText(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	accountID := apikeys.Current(c).AccountID
	cost := jobCost(text)
	var id string
	err := t.credits.WithAccount(ctx, accountID, func(tx *credits.Tx) error {
		avail, err := tx.Available()
		if err != nil {
			return err
		}
		if cost > avail {
			return credits.ErrInsufficient
		}
		return tx.SQL().QueryRowContext(ctx, `
			INSERT INTO jobs (account_id, input, reserved_cents)
			VALUES ($1, $2, $3)
			RETURNING id`, accountID, text, cost,
		).Scan(&id)
	})
	if errors.Is(err, credits.ErrInsufficient) {
		apiError(c, http.StatusPaymentRequired, "insufficient_credit", "Buy credit to continue.")
		return
	}
	if err != nil {
		web.Log(c).Error("creating a job failed", "error", err)
		apiError(c, http.StatusServiceUnavailable, "unavailable", "Try again in a moment.")
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"id": id, "reserved_cents": cost})
}

var errJobNotFound = errors.New("job not found")

// completeJob releases the hold and charges the job in one transaction. Completing a
// job twice charges it once.
func (t *textlab) completeJob(c *gin.Context) {
	ctx := c.Request.Context()
	accountID := apikeys.Current(c).AccountID
	id := c.Param("id")
	var charged int64
	err := t.credits.WithAccount(ctx, accountID, func(tx *credits.Tx) error {
		var status string
		err := tx.SQL().QueryRowContext(ctx, `
			SELECT status, reserved_cents
			FROM jobs
			WHERE id::text = $1 AND account_id = $2
			FOR UPDATE`, id, accountID,
		).Scan(&status, &charged)
		if errors.Is(err, sql.ErrNoRows) {
			return errJobNotFound
		}
		if err != nil || status == "done" {
			return err
		}
		if _, err := tx.SQL().ExecContext(ctx, `
			UPDATE jobs
			SET status = 'done', charged_cents = reserved_cents, done_at = now()
			WHERE id::text = $1`, id,
		); err != nil {
			return err
		}
		return tx.Debit(charged, "job:"+id, "Text job")
	})
	if errors.Is(err, errJobNotFound) {
		apiError(c, http.StatusNotFound, "not_found", "No such job.")
		return
	}
	if err != nil {
		web.Log(c).Error("completing a job failed", "error", err)
		apiError(c, http.StatusServiceUnavailable, "unavailable", "Try again in a moment.")
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": id, "status": "done", "charged_cents": charged})
}
