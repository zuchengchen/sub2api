package service

import (
	"context"
	"errors"
	"strings"
)

type registrationAdmission struct {
	invitationRedeemCode *RedeemCode
	affiliateCode        string
}

func (a *registrationAdmission) effectiveAffiliateCode(fallback string) string {
	if a != nil && a.affiliateCode != "" {
		return a.affiliateCode
	}
	return strings.TrimSpace(fallback)
}

func (s *AuthService) validateRegistrationAdmission(ctx context.Context, rawCode string) (*registrationAdmission, error) {
	admission := &registrationAdmission{}
	if s == nil || s.settingService == nil || !s.settingService.IsInvitationCodeEnabled(ctx) {
		return admission, nil
	}

	code := strings.TrimSpace(rawCode)
	if code == "" {
		return nil, ErrInvitationCodeRequired
	}

	redeemLookupAvailable := s.redeemRepo != nil || s.oauthEmailFlowClient(ctx) != nil
	redeemLookupFailed := false
	if redeemLookupAvailable {
		redeemCode, err := s.loadOAuthRegistrationInvitation(ctx, code)
		switch {
		case err == nil && redeemCode != nil && redeemCode.Type == RedeemTypeInvitation && redeemCode.CanUse():
			admission.invitationRedeemCode = redeemCode
			return admission, nil
		case err != nil && !errors.Is(err, ErrRedeemCodeNotFound):
			redeemLookupFailed = true
		}
	}

	affiliateLookupAvailable := s.affiliateService != nil && s.affiliateService.IsEnabled(ctx)
	if affiliateLookupAvailable {
		affiliate, err := s.affiliateService.ValidateAffiliateCode(ctx, code)
		if err == nil {
			if s.userRepo == nil {
				return nil, ErrServiceUnavailable
			}
			owner, ownerErr := s.userRepo.GetByID(ctx, affiliate.UserID)
			switch {
			case ownerErr == nil && owner != nil && owner.IsActive():
				admission.affiliateCode = strings.ToUpper(code)
				return admission, nil
			case ownerErr == nil || errors.Is(ownerErr, ErrUserNotFound):
				return nil, ErrInvitationCodeInvalid
			default:
				return nil, ErrServiceUnavailable
			}
		}
		if !errors.Is(err, ErrAffiliateCodeInvalid) {
			return nil, ErrServiceUnavailable
		}
	}

	if redeemLookupFailed || (!redeemLookupAvailable && !affiliateLookupAvailable) {
		return nil, ErrServiceUnavailable
	}
	return nil, ErrInvitationCodeInvalid
}

// ValidateRegistrationAdmissionCode validates the public registration gate.
// The existing invitation-code endpoint calls this method for compatibility.
func (s *AuthService) ValidateRegistrationAdmissionCode(ctx context.Context, code string) error {
	_, err := s.validateRegistrationAdmission(ctx, code)
	return err
}
