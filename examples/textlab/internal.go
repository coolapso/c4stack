package main

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/coolapso/c4stack/subscriptions"
	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
)

// internalAPI is Textlab's API for other services of yours, such as a staff app: the
// subscription reviews, to show and resolve. It runs on a server of its own
// (TEXTLAB_INTERNAL_ADDR), never through the public ingress, and takes a bearer token
// whose SHA-256 Textlab keeps (TEXTLAB_INTERNAL_TOKEN_SHA256). subs is nil when
// Textlab sells no plans: there are no reviews then.
func internalAPI(cfg config, subs *subscriptions.Subscriptions) (http.Handler, error) {
	r, err := web.NewInternal(web.InternalConfig{TokenHashes: cfg.InternalTokenHashes})
	if err != nil {
		return nil, err
	}
	v1 := r.Group("/v1", web.MaxBody(16<<10))
	v1.GET("/reviews", func(c *gin.Context) {
		out := []reviewView{}
		if subs != nil {
			reviews, err := subs.Reviews(c.Request.Context())
			if err != nil {
				web.Log(c).Error("listing reviews", "error", err)
				c.Status(http.StatusInternalServerError)
				return
			}
			for _, rv := range reviews {
				out = append(out, reviewView{ID: rv.ID, EventID: rv.EventID, SubscriptionID: rv.SubscriptionID,
					AccountID: rv.AccountID, Reason: rv.Reason, CreatedAt: rv.CreatedAt})
			}
		}
		c.JSON(http.StatusOK, gin.H{"reviews": out})
	})
	v1.POST("/reviews/:id/resolve", func(c *gin.Context) {
		var body struct {
			Note string `json:"note"`
		}
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || subs == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "no open review has that ID"})
			return
		}
		if err := c.ShouldBindJSON(&body); err != nil || body.Note == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "send a note on what was done"})
			return
		}
		switch err := subs.Resolve(c.Request.Context(), id, body.Note); {
		case errors.Is(err, subscriptions.ErrNoReview):
			c.JSON(http.StatusNotFound, gin.H{"error": "no open review has that ID"})
		case err != nil:
			web.Log(c).Error("resolving a review", "error", err)
			c.Status(http.StatusInternalServerError)
		default:
			c.Status(http.StatusNoContent)
		}
	})
	return r, nil
}

// reviewView is a subscription review as the internal API gives it.
type reviewView struct {
	ID             int64     `json:"id"`
	EventID        string    `json:"event_id"`
	SubscriptionID string    `json:"subscription_id,omitempty"`
	AccountID      string    `json:"account_id,omitempty"`
	Reason         string    `json:"reason"`
	CreatedAt      time.Time `json:"created_at"`
}
