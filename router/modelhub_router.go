// Modelhub router — S11 + S12.
//
// Mounts the canonical /v1/auth/* + /v1/wallet/* + /admin/wallet/*
// surface on top of the inherited new-api session middleware. Kept in a
// dedicated file so the inherited router/api-router.go stays unchanged.
//
// All routes go through ModelhubCORS so the SPA at localhost:5173 (dev) can
// hit the API with credentials.

package router

import (
	"github.com/QuantumNous/new-api/controller"
	modelhubapi "github.com/QuantumNous/new-api/internal/api"
	"github.com/QuantumNous/new-api/middleware"

	"github.com/gin-gonic/gin"
)

// SetModelhubRouter wires the modelhub-specific endpoints. Called from
// router.SetRouter alongside the inherited route setups.
func SetModelhubRouter(router *gin.Engine) {
	cors := middleware.ModelhubCORS()

	// /v1/auth — register/login/logout (modelhub-shaped {email,password}
	// wrappers over the inherited new-api user model) + identity probe.
	// register/login/logout share the same session store as the inherited
	// /api/user/* routes, so accounts created here also work in the admin UI.
	v1Auth := router.Group("/v1/auth")
	v1Auth.Use(cors)
	{
		v1Auth.POST("/register", controller.ModelhubRegister)
		v1Auth.POST("/login", controller.ModelhubLogin)
		v1Auth.POST("/logout", controller.ModelhubLogout)
		// ModelhubUserAuth (cookie-only) instead of middleware.UserAuth
		// (cookie + New-Api-User header). The SPA in modelhub-web is
		// cookie-only by design — see ModelhubUserAuth godoc.
		v1Auth.GET("/me", controller.ModelhubUserAuth(), controller.AuthMe)
	}

	// /v1/wallet — self-only views. Same session cookie.
	v1Wallet := router.Group("/v1/wallet")
	v1Wallet.Use(cors)
	v1Wallet.Use(controller.ModelhubUserAuth())
	{
		v1Wallet.GET("/balance", controller.WalletBalanceSelf)
		v1Wallet.GET("/history", controller.WalletHistorySelf)
	}

	// Modelhub generation surface. /v1/models is already owned by relay.
	v1Modelhub := router.Group("/v1")
	v1Modelhub.Use(cors)
	v1Modelhub.Use(controller.ModelhubUserAuth())
	{
		v1Modelhub.GET("/modelhub/models", controller.ModelhubListModels)
		v1Modelhub.POST("/uploads", gin.WrapF(modelhubapi.CreateUpload()))
		v1Modelhub.POST("/generations", controller.ModelhubSubmitGeneration)
		v1Modelhub.GET("/generations/:id", controller.ModelhubGetGeneration)
	}

	// /admin/wallet — admin-only top-up + history view.
	adminWallet := router.Group("/admin/wallet")
	adminWallet.Use(cors)
	adminWallet.Use(middleware.AdminAuth())
	{
		adminWallet.POST("/topup", controller.AdminWalletTopup)
		adminWallet.GET("/history", controller.AdminWalletHistory)
	}
}
