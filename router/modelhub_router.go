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
	"github.com/QuantumNous/new-api/middleware"

	"github.com/gin-gonic/gin"
)

// SetModelhubRouter wires the modelhub-specific endpoints. Called from
// router.SetRouter alongside the inherited route setups.
func SetModelhubRouter(router *gin.Engine) {
	cors := middleware.ModelhubCORS()

	// /v1/auth — auth identity probe. Login/Register stay on the inherited
	// /api/user/login + /api/user/register (S11 doesn't replace them, just
	// adds the modelhub-shaped /v1 endpoints in front of the same session
	// store).
	v1Auth := router.Group("/v1/auth")
	v1Auth.Use(cors)
	{
		v1Auth.GET("/me", middleware.UserAuth(), controller.AuthMe)
	}

	// /v1/wallet — self-only views. Same session cookie.
	v1Wallet := router.Group("/v1/wallet")
	v1Wallet.Use(cors)
	v1Wallet.Use(middleware.UserAuth())
	{
		v1Wallet.GET("/balance", controller.WalletBalanceSelf)
		v1Wallet.GET("/history", controller.WalletHistorySelf)
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
