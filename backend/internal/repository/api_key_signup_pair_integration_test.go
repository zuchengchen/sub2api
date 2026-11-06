//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/ent/apikey"
	"github.com/Wei-Shaw/sub2api/ent/userallowedgroup"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSignupGPTKeysProvisionPostgres(t *testing.T) {
	for _, mode := range []string{"success", "missing_group", "second_insert_failure"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			client := testEntClient(t)
			groupIDs := map[string]int64{}
			for _, name := range []string{service.TiboHealthyGroupName, service.TiboWelfareGroupName} {
				if mode == "missing_group" && name == service.TiboWelfareGroupName {
					continue
				}
				group := mustCreateGroup(t, client, &service.Group{Name: name, Platform: service.PlatformOpenAI, IsExclusive: true})
				groupIDs[name] = group.ID
				t.Cleanup(func() {
					_, err := integrationDB.ExecContext(ctx, "DELETE FROM groups WHERE id=$1", group.ID)
					require.NoError(t, err)
				})
			}
			user := mustCreateUser(t, client, &service.User{})
			t.Cleanup(func() {
				_, err := integrationDB.ExecContext(ctx, "DELETE FROM users WHERE id=$1", user.ID)
				require.NoError(t, err)
			})
			if mode == "second_insert_failure" {
				_, err := integrationDB.ExecContext(ctx, `CREATE FUNCTION test_signup_pair_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
					IF NEW.name='GPT-福利' THEN RAISE EXCEPTION 'test signup second insert failure'; END IF;
					RETURN NEW; END $$;
					CREATE TRIGGER test_signup_pair_failure BEFORE INSERT ON api_keys FOR EACH ROW EXECUTE FUNCTION test_signup_pair_failure()`)
				require.NoError(t, err)
				t.Cleanup(func() {
					_, err := integrationDB.ExecContext(ctx, `DROP TRIGGER IF EXISTS test_signup_pair_failure ON api_keys; DROP FUNCTION IF EXISTS test_signup_pair_failure()`)
					require.NoError(t, err)
				})
			}
			repo := newAPIKeyRepositoryWithSQL(client, integrationDB)
			svc := service.NewAPIKeyService(repo, nil, newGroupRepositoryWithSQL(client, integrationDB), nil, nil, nil, nil)
			err := svc.ProvisionDefaultSignupAPIKey(ctx, user.ID)
			want := 2
			if mode != "success" {
				require.Error(t, err)
				want = 0
			} else {
				require.NoError(t, err)
			}
			keys, err := client.APIKey.Query().Where(apikey.UserIDEQ(user.ID)).All(ctx)
			require.NoError(t, err)
			require.Len(t, keys, want)
			grants, err := client.UserAllowedGroup.Query().Where(userallowedgroup.UserIDEQ(user.ID)).All(ctx)
			require.NoError(t, err)
			require.Len(t, grants, want)
			for _, key := range keys {
				require.Contains(t, groupIDs, key.Name)
				require.NotNil(t, key.GroupID)
				require.Equal(t, groupIDs[key.Name], *key.GroupID)
				require.Equal(t, service.StatusActive, key.Status)
			}
			if mode == "success" {
				require.NotEqual(t, keys[0].Key, keys[1].Key)
			}
			registered, err := client.User.Get(ctx, user.ID)
			require.NoError(t, err)
			require.Equal(t, user.ID, registered.ID, "key bootstrap must not remove the registered user")
		})
	}
}
