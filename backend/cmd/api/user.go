// Package api (user) contains handlers for user data.
package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/ALCOpenSource/Mentor-Management-System-Team-7/backend/db"
	"github.com/ALCOpenSource/Mentor-Management-System-Team-7/backend/db/models"
	"github.com/ALCOpenSource/Mentor-Management-System-Team-7/backend/internal/token"
	"github.com/ALCOpenSource/Mentor-Management-System-Team-7/backend/internal/utils"
	"github.com/ALCOpenSource/Mentor-Management-System-Team-7/backend/internal/worker"
	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
	"github.com/hibiken/asynq"
	"github.com/rs/zerolog/log"
)

type changeUserPasswordRequest struct {
	CurrentPassword string `json:"current_password" binding:"required,min=8"`
	NewPassword     string `json:"new_password" binding:"required,min=8"`
	ConfirmPassword string `json:"confirm_new_password" binding:"required,min=8,eqfield=NewPassword"`
}

type changeUserPasswordRequestID struct {
	ID string `uri:"id" binding:"required,min=1"`
}

func (server *Server) changeUserPassword(ctx *gin.Context) {
	var reqID changeUserPasswordRequestID
	if err := ctx.ShouldBindUri(&reqID); err != nil {
		ctx.JSON(http.StatusBadRequest, errorResponse(err))
		return
	}

	authPayload := ctx.MustGet(authorizationPayloadKey).(*token.Payload)

	if reqID.ID != authPayload.UserID {
		err := errors.New("mismatched user")
		ctx.JSON(http.StatusUnauthorized, errorResponse(err))
	}

	var req changeUserPasswordRequest
	if err := BindJSONWithValidation(ctx, &req, validator.New()); err != nil {
		return
	}

	user, err := server.store.GetUser(ctx, authPayload.UserID)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrRecordNotFound):
			ctx.JSON(http.StatusNotFound, errorResponse(err))
		default:
			ctx.JSON(http.StatusInternalServerError, errorResponse(err))
		}
		return
	}

	err = utils.CheckPassword(req.CurrentPassword, user.HashedPassword)
	if err != nil {
		ctx.JSON(http.StatusUnauthorized, errorResponse(err))
		return
	}

	hashedPassword, err := utils.HashedPassword(req.NewPassword)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, errorResponse(err))
		return
	}

	updateUserParams := map[string]interface{}{
		"hashed_password":     hashedPassword,
		"password_changed_at": time.Now(),
	}

	_, err = server.store.UpdateUser(ctx, authPayload.UserID, updateUserParams)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, errorResponse(err))
		return
	}

	ctx.JSON(http.StatusOK, gin.H{"result": "password changed successfully"})
	log.Info().
		Str("user_id", user.ID.Hex()).
		Str("ip_address", ctx.ClientIP()).
		Str("user_agent", ctx.Request.UserAgent()).
		Str("request_method", ctx.Request.Method).
		Str("request_path", ctx.Request.URL.Path).
		Msg("password changed for user")
}

type forgotPasswordRequest struct {
	Email string `json:"email" binding:"required,email"`
}

func (server *Server) forgotPassword(ctx *gin.Context) {
	var req forgotPasswordRequest

	if err := BindJSONWithValidation(ctx, &req, validator.New()); err != nil {
		return
	}

	user, err := server.store.GetUserByEmail(ctx, req.Email)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrRecordNotFound):
			ctx.JSON(http.StatusNotFound, errorResponse(err))
		default:
			ctx.JSON(http.StatusInternalServerError, errorResponse(err))
		}
		return
	}

	now := time.Now()
	resetPassword, err := server.store.CreateUserAction(ctx, &models.UserAction{
		UserID:     user.ID,
		Email:      user.Contact.Email,
		SecretCode: utils.RandomString(64), // TODO: Substitute value with a token-based string
		ActionType: "reset_password",
		CreatedAt:  now,
		ExpiredAt:  now.Add(15 * time.Minute),
	})
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, errorResponse(err))
		return
	}

	task := &worker.PayloadResetPasswordEmail{
		ID:        resetPassword.ID.Hex(),
		UserID:    user.ID.Hex(),
		UserEmail: user.Contact.Email,
	}
	opts := []asynq.Option{
		asynq.MaxRetry(10),
		asynq.ProcessIn(5 * time.Second),
		asynq.Queue(worker.QueueCritical),
	}
	err = server.taskDistributor.DistributeTaskSendResetPasswordEmail(ctx, task, opts...)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, errorResponse(err))
		log.Error().Err(err).
			Str("user_id", user.ID.Hex()).
			Str("request_method", ctx.Request.Method).
			Str("request_path", ctx.Request.URL.Path).
			Msg("task 'reset password' failed to enqueued")
		return
	}

	ctx.JSON(http.StatusOK, envelop{"result": "reset password email sent"})

	log.Info().
		Str("user_id", user.ID.Hex()).
		Str("ip_address", ctx.ClientIP()).
		Str("user_agent", ctx.Request.UserAgent()).
		Str("request_method", ctx.Request.Method).
		Str("request_path", ctx.Request.URL.Path).
		Msg("task 'reset password' enqueued")
}

// Login
type userLogin struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required,min=8"`
}

func (server *Server) login(ctx *gin.Context) {
	var req userLogin
	// Validate request.
	if err := BindJSONWithValidation(ctx, &req, validator.New()); err != nil {
		return
	}
	// Get user by email.
	user, err := server.store.GetUserByEmail(ctx, req.Email)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrRecordNotFound):
			ctx.JSON(http.StatusNotFound, errorResponse(err))
		default:
			ctx.JSON(http.StatusInternalServerError, errorResponse(err))
		}
		return
	}
	// Check password.
	err = utils.CheckPassword(req.Password, user.HashedPassword)
	if err != nil {
		ctx.JSON(http.StatusUnauthorized, errorResponse(err))
		return
	}
	// Create token.
	token, payload, err := server.tokenMaker.CreateToken(user.ID.Hex(), user.Role, 24*time.Hour)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, errorResponse(err))
		return
	}

	// Return token.
	ctx.JSON(http.StatusOK,
		envelop{
			"data": gin.H{
				"token":   token,
				"payload": payload,
			},
		},
	)
// Log user login where 
	log.Info().
		Str("user_id", user.ID.Hex()).
		Str("ip_address", ctx.ClientIP()).
		Str("user_agent", ctx.Request.UserAgent()).
		Str("request_method", ctx.Request.Method).
		Str("request_path", ctx.Request.URL.Path).
		Msg("user logged in")
}
