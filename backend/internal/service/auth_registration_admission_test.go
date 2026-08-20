//go:build unit

package service

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type admissionAffiliateBindCall struct {
	userID    int64
	inviterID int64
}

type admissionAffiliateRepoStub struct {
	AffiliateRepository
	codeOwners map[string]int64
	ensureIDs  []int64
	bindCalls  []admissionAffiliateBindCall
}

func (r *admissionAffiliateRepoStub) EnsureUserAffiliate(_ context.Context, userID int64) (*AffiliateSummary, error) {
	r.ensureIDs = append(r.ensureIDs, userID)
	return &AffiliateSummary{UserID: userID, AffCode: "SELF"}, nil
}

func (r *admissionAffiliateRepoStub) GetAffiliateByCode(_ context.Context, code string) (*AffiliateSummary, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	userID, ok := r.codeOwners[code]
	if !ok {
		return nil, ErrAffiliateProfileNotFound
	}
	return &AffiliateSummary{UserID: userID, AffCode: code}, nil
}

func (r *admissionAffiliateRepoStub) BindInviter(_ context.Context, userID, inviterID int64) (bool, error) {
	r.bindCalls = append(r.bindCalls, admissionAffiliateBindCall{userID: userID, inviterID: inviterID})
	return true, nil
}

func newAdmissionAuthService(
	userRepo *userRepoStub,
	redeemRepo RedeemCodeRepository,
	affiliateRepo *admissionAffiliateRepoStub,
	affiliateEnabled bool,
) *AuthService {
	settings := map[string]string{
		SettingKeyRegistrationEnabled:   "true",
		SettingKeyInvitationCodeEnabled: "true",
		SettingKeyAffiliateEnabled:      "false",
	}
	if affiliateEnabled {
		settings[SettingKeyAffiliateEnabled] = "true"
	}

	authService := newAuthService(userRepo, settings, nil, nil)
	authService.redeemRepo = redeemRepo
	if affiliateRepo != nil {
		authService.affiliateService = NewAffiliateService(affiliateRepo, authService.settingService, nil, nil)
	}
	return authService
}

func TestValidateRegistrationAdmissionAcceptsAffiliateCode(t *testing.T) {
	const inviterID int64 = 91
	userRepo := &userRepoStub{usersByID: map[int64]*User{
		inviterID: {ID: inviterID, Status: StatusActive},
	}}
	affiliateRepo := &admissionAffiliateRepoStub{codeOwners: map[string]int64{
		"FGDZC7AJ7ZKZ": inviterID,
	}}
	authService := newAdmissionAuthService(userRepo, &redeemCodeRepoStub{}, affiliateRepo, true)

	admission, err := authService.validateRegistrationAdmission(context.Background(), " fgdzc7aj7zkz ")

	require.NoError(t, err)
	require.Nil(t, admission.invitationRedeemCode)
	require.Equal(t, "FGDZC7AJ7ZKZ", admission.affiliateCode)
}

func TestValidateRegistrationAdmissionRejectsAffiliateCodeWhenFeatureDisabled(t *testing.T) {
	const inviterID int64 = 91
	userRepo := &userRepoStub{usersByID: map[int64]*User{
		inviterID: {ID: inviterID, Status: StatusActive},
	}}
	affiliateRepo := &admissionAffiliateRepoStub{codeOwners: map[string]int64{
		"FGDZC7AJ7ZKZ": inviterID,
	}}
	authService := newAdmissionAuthService(userRepo, &redeemCodeRepoStub{}, affiliateRepo, false)

	_, err := authService.validateRegistrationAdmission(context.Background(), "FGDZC7AJ7ZKZ")

	require.ErrorIs(t, err, ErrInvitationCodeInvalid)
}

func TestValidateRegistrationAdmissionRejectsInactiveAffiliateOwner(t *testing.T) {
	const inviterID int64 = 91
	userRepo := &userRepoStub{usersByID: map[int64]*User{
		inviterID: {ID: inviterID, Status: StatusDisabled},
	}}
	affiliateRepo := &admissionAffiliateRepoStub{codeOwners: map[string]int64{
		"FGDZC7AJ7ZKZ": inviterID,
	}}
	authService := newAdmissionAuthService(userRepo, &redeemCodeRepoStub{}, affiliateRepo, true)

	_, err := authService.validateRegistrationAdmission(context.Background(), "FGDZC7AJ7ZKZ")

	require.ErrorIs(t, err, ErrInvitationCodeInvalid)
}

func TestRegisterWithAffiliateAdmissionBindsAdmissionCodeOwner(t *testing.T) {
	const (
		newUserID int64 = 42
		inviterID int64 = 91
		otherID   int64 = 92
	)
	userRepo := &userRepoStub{
		nextID: newUserID,
		usersByID: map[int64]*User{
			inviterID: {ID: inviterID, Status: StatusActive},
			otherID:   {ID: otherID, Status: StatusActive},
		},
	}
	affiliateRepo := &admissionAffiliateRepoStub{codeOwners: map[string]int64{
		"FGDZC7AJ7ZKZ": inviterID,
		"OTHER-CODE":   otherID,
	}}
	redeemRepo := &redeemCodeRepoStub{}
	authService := newAdmissionAuthService(userRepo, redeemRepo, affiliateRepo, true)

	_, user, err := authService.RegisterWithVerification(
		context.Background(),
		"new@example.com",
		"password",
		"",
		"FGDZC7AJ7ZKZ",
		"OTHER-CODE",
	)

	require.NoError(t, err)
	require.Equal(t, newUserID, user.ID)
	require.Equal(t, []admissionAffiliateBindCall{{userID: newUserID, inviterID: inviterID}}, affiliateRepo.bindCalls)
	require.Empty(t, redeemRepo.useCalls)
}

func TestOAuthRegistrationWithAffiliateAdmissionBindsAdmissionCodeOwner(t *testing.T) {
	const (
		newUserID int64 = 43
		inviterID int64 = 91
		otherID   int64 = 92
	)
	userRepo := &userRepoStub{
		nextID: newUserID,
		usersByID: map[int64]*User{
			inviterID: {ID: inviterID, Status: StatusActive},
			otherID:   {ID: otherID, Status: StatusActive},
		},
	}
	affiliateRepo := &admissionAffiliateRepoStub{codeOwners: map[string]int64{
		"FGDZC7AJ7ZKZ": inviterID,
		"OTHER-CODE":   otherID,
	}}
	authService := newAdmissionAuthService(userRepo, &redeemCodeRepoStub{}, affiliateRepo, true)
	authService.refreshTokenCache = &refreshTokenCacheStub{}

	tokenPair, user, err := authService.LoginOrRegisterOAuthWithTokenPair(
		context.Background(),
		"oauth-new@example.com",
		"oauth-new",
		"FGDZC7AJ7ZKZ",
		"OTHER-CODE",
		"oidc",
	)

	require.NoError(t, err)
	require.NotNil(t, tokenPair)
	require.Equal(t, newUserID, user.ID)
	require.Equal(t, []admissionAffiliateBindCall{{userID: newUserID, inviterID: inviterID}}, affiliateRepo.bindCalls)
}
