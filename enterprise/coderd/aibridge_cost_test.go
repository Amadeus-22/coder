package coderd_test

import (
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd/coderdtest"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbgen"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/coderd/rbac"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/enterprise/coderd/coderdenttest"
	"github.com/coder/coder/v2/enterprise/coderd/license"
	"github.com/coder/coder/v2/testutil"
	"github.com/coder/serpent"
)

func aiCostOpts(t *testing.T) *coderdenttest.Options {
	t.Helper()
	dv := coderdtest.DeploymentValues(t)
	dv.AI.BridgeConfig.Enabled = serpent.Bool(true)
	dv.Experiments = []string{string(codersdk.ExperimentAIGatewayCostControl)}
	return &coderdenttest.Options{
		Options: &coderdtest.Options{
			DeploymentValues: dv,
		},
		LicenseOptions: &coderdenttest.LicenseOptions{
			Features: license.Features{
				codersdk.FeatureAIBridge: 1,
			},
		},
	}
}

// seedCostData inserts two interceptions with token usages for the given
// user: one priced Coder Agents request linked to a chat, and one unpriced
// request from another client.
func seedCostData(t *testing.T, db database.Store, orgID, userID uuid.UUID, now time.Time) (chatID uuid.UUID) {
	t.Helper()

	modelConfig := dbgen.ChatModelConfig(t, db, database.ChatModelConfig{})
	chat := dbgen.Chat(t, db, database.Chat{
		OrganizationID:    orgID,
		OwnerID:           userID,
		LastModelConfigID: modelConfig.ID,
		Title:             "cost test chat",
	})

	agentsEndedAt := now.Add(time.Second)
	agentsInterception := dbgen.AIBridgeInterception(t, db, database.InsertAIBridgeInterceptionParams{
		InitiatorID:     userID,
		Provider:        "anthropic",
		Model:           "claude-4",
		StartedAt:       now,
		Client:          sql.NullString{String: "Coder Agents", Valid: true},
		ClientSessionID: sql.NullString{String: chat.ID.String(), Valid: true},
	}, &agentsEndedAt)
	dbgen.AIBridgeTokenUsage(t, db, database.InsertAIBridgeTokenUsageParams{
		InterceptionID:        agentsInterception.ID,
		InputTokens:           100,
		OutputTokens:          50,
		CacheReadInputTokens:  10,
		CacheWriteInputTokens: 5,
		CostMicros:            sql.NullInt64{Int64: 1_500, Valid: true},
		CreatedAt:             now,
	})

	otherEndedAt := now.Add(time.Minute + time.Second)
	otherInterception := dbgen.AIBridgeInterception(t, db, database.InsertAIBridgeInterceptionParams{
		InitiatorID: userID,
		Provider:    "openai",
		Model:       "gpt-5",
		StartedAt:   now.Add(time.Minute),
		Client:      sql.NullString{String: "claude-code", Valid: true},
	}, &otherEndedAt)
	dbgen.AIBridgeTokenUsage(t, db, database.InsertAIBridgeTokenUsageParams{
		InterceptionID: otherInterception.ID,
		InputTokens:    200,
		OutputTokens:   80,
		CreatedAt:      now.Add(time.Minute),
	})

	return chat.ID
}

func TestUserAICostSummary(t *testing.T) {
	t.Parallel()

	t.Run("RequiresLicenseFeature", func(t *testing.T) {
		t.Parallel()

		dv := coderdtest.DeploymentValues(t)
		dv.Experiments = []string{string(codersdk.ExperimentAIGatewayCostControl)}
		client, _ := coderdenttest.New(t, &coderdenttest.Options{
			Options:        &coderdtest.Options{DeploymentValues: dv},
			LicenseOptions: &coderdenttest.LicenseOptions{Features: license.Features{}},
		})
		ctx := testutil.Context(t, testutil.WaitLong)

		//nolint:gocritic // Owner role is irrelevant here; the request is blocked before RBAC.
		_, err := client.UserAICostSummary(ctx, uuid.New(), codersdk.AIBridgeCostFilter{})
		var sdkErr *codersdk.Error
		require.ErrorAs(t, err, &sdkErr)
		require.Equal(t, http.StatusForbidden, sdkErr.StatusCode())
	})

	t.Run("OK", func(t *testing.T) {
		t.Parallel()
		client, db, firstUser := coderdenttest.NewWithDatabase(t, aiCostOpts(t))
		ctx := testutil.Context(t, testutil.WaitLong)

		now := dbtime.Now()
		chatID := seedCostData(t, db, firstUser.OrganizationID, firstUser.UserID, now)

		//nolint:gocritic // Testing aggregate correctness, not authz.
		got, err := client.UserAICostSummary(ctx, firstUser.UserID, codersdk.AIBridgeCostFilter{
			StartDate: now.Add(-time.Hour),
			EndDate:   now.Add(time.Hour),
		})
		require.NoError(t, err)

		require.EqualValues(t, 1_500, got.TotalCostMicros)
		require.EqualValues(t, 2, got.RequestCount)
		require.EqualValues(t, 1, got.PricedRequestCount)
		require.EqualValues(t, 1, got.UnpricedRequestCount)
		require.EqualValues(t, 300, got.TotalInputTokens)
		require.EqualValues(t, 130, got.TotalOutputTokens)
		require.EqualValues(t, 10, got.TotalCacheReadTokens)
		require.EqualValues(t, 5, got.TotalCacheWriteTokens)

		require.Len(t, got.ByModel, 2)
		require.Equal(t, "anthropic", got.ByModel[0].Provider)
		require.Equal(t, "claude-4", got.ByModel[0].Model)
		require.EqualValues(t, 1_500, got.ByModel[0].TotalCostMicros)
		require.EqualValues(t, 1, got.ByModel[0].RequestCount)
		require.Equal(t, "openai", got.ByModel[1].Provider)
		require.EqualValues(t, 0, got.ByModel[1].TotalCostMicros)
		require.EqualValues(t, 1, got.ByModel[1].UnpricedRequestCount)

		require.Len(t, got.ByChat, 1)
		require.Equal(t, chatID, got.ByChat[0].ChatID)
		require.Equal(t, "cost test chat", got.ByChat[0].ChatTitle)
		require.EqualValues(t, 1_500, got.ByChat[0].TotalCostMicros)
		require.EqualValues(t, 1, got.ByChat[0].RequestCount)
	})

	t.Run("ClientFilter", func(t *testing.T) {
		t.Parallel()
		client, db, firstUser := coderdenttest.NewWithDatabase(t, aiCostOpts(t))
		ctx := testutil.Context(t, testutil.WaitLong)

		now := dbtime.Now()
		chatID := seedCostData(t, db, firstUser.OrganizationID, firstUser.UserID, now)

		// A different client reusing the same chat session ID must be
		// excluded from every filtered aggregate, including by_chat.
		otherClientEndedAt := now.Add(2*time.Minute + time.Second)
		otherClientInterception := dbgen.AIBridgeInterception(t, db, database.InsertAIBridgeInterceptionParams{
			InitiatorID:     firstUser.UserID,
			Provider:        "openai",
			Model:           "gpt-5",
			StartedAt:       now.Add(2 * time.Minute),
			Client:          sql.NullString{String: "claude-code", Valid: true},
			ClientSessionID: sql.NullString{String: chatID.String(), Valid: true},
		}, &otherClientEndedAt)
		dbgen.AIBridgeTokenUsage(t, db, database.InsertAIBridgeTokenUsageParams{
			InterceptionID: otherClientInterception.ID,
			InputTokens:    500,
			OutputTokens:   200,
			CostMicros:     sql.NullInt64{Int64: 9_000, Valid: true},
			CreatedAt:      now.Add(2 * time.Minute),
		})

		//nolint:gocritic // Testing filter correctness, not authz.
		got, err := client.UserAICostSummary(ctx, firstUser.UserID, codersdk.AIBridgeCostFilter{
			StartDate: now.Add(-time.Hour),
			EndDate:   now.Add(time.Hour),
			Client:    "Coder Agents",
		})
		require.NoError(t, err)
		require.EqualValues(t, 1, got.RequestCount)
		require.EqualValues(t, 1_500, got.TotalCostMicros)
		require.Len(t, got.ByModel, 1)
		require.Equal(t, "claude-4", got.ByModel[0].Model)
		require.Len(t, got.ByChat, 1)
		require.Equal(t, chatID, got.ByChat[0].ChatID)
		require.EqualValues(t, 1, got.ByChat[0].RequestCount)
		require.EqualValues(t, 1_500, got.ByChat[0].TotalCostMicros)
	})

	t.Run("DateRangeExcludes", func(t *testing.T) {
		t.Parallel()
		client, db, firstUser := coderdenttest.NewWithDatabase(t, aiCostOpts(t))
		ctx := testutil.Context(t, testutil.WaitLong)

		now := dbtime.Now()
		seedCostData(t, db, firstUser.OrganizationID, firstUser.UserID, now)

		//nolint:gocritic // Testing filter correctness, not authz.
		got, err := client.UserAICostSummary(ctx, firstUser.UserID, codersdk.AIBridgeCostFilter{
			StartDate: now.Add(-2 * time.Hour),
			EndDate:   now.Add(-time.Hour),
		})
		require.NoError(t, err)
		require.EqualValues(t, 0, got.RequestCount)
		require.EqualValues(t, 0, got.TotalCostMicros)
		require.Empty(t, got.ByModel)
		require.Empty(t, got.ByChat)
	})

	t.Run("ChatBreakdownRequiresChatRead", func(t *testing.T) {
		t.Parallel()
		ownerClient, db, firstUser := coderdenttest.NewWithDatabase(t, aiCostOpts(t))
		userAdminClient, _ := coderdtest.CreateAnotherUser(t, ownerClient, firstUser.OrganizationID, rbac.RoleUserAdmin())
		ctx := testutil.Context(t, testutil.WaitLong)

		now := dbtime.Now()
		seedCostData(t, db, firstUser.OrganizationID, firstUser.UserID, now)

		filter := codersdk.AIBridgeCostFilter{
			StartDate: now.Add(-time.Hour),
			EndDate:   now.Add(time.Hour),
		}
		// User admins can read users but not their chats: totals and the
		// model breakdown are returned, chat titles are omitted.
		got, err := userAdminClient.UserAICostSummary(ctx, firstUser.UserID, filter)
		require.NoError(t, err)
		require.EqualValues(t, 2, got.RequestCount)
		require.NotEmpty(t, got.ByModel)
		require.Empty(t, got.ByChat)

		//nolint:gocritic // Contrast case: owners hold chat read.
		got, err = ownerClient.UserAICostSummary(ctx, firstUser.UserID, filter)
		require.NoError(t, err)
		require.NotEmpty(t, got.ByChat)
	})

	t.Run("RoleAccess", func(t *testing.T) {
		t.Parallel()

		ownerClient, owner := coderdenttest.New(t, aiCostOpts(t))
		memberClient, memberUser := coderdtest.CreateAnotherUser(t, ownerClient, owner.OrganizationID)
		userAdminClient, _ := coderdtest.CreateAnotherUser(t, ownerClient, owner.OrganizationID, rbac.RoleUserAdmin())
		_, targetUser := coderdtest.CreateAnotherUser(t, ownerClient, owner.OrganizationID)

		cases := []struct {
			Name     string
			Client   *codersdk.Client
			Target   uuid.UUID
			WantCode int
		}{
			{Name: "Owner", Client: ownerClient, Target: targetUser.ID, WantCode: http.StatusOK},
			{Name: "UserAdmin", Client: userAdminClient, Target: targetUser.ID, WantCode: http.StatusOK},
			{Name: "MemberReadsSelf", Client: memberClient, Target: memberUser.ID, WantCode: http.StatusOK},
			{Name: "MemberReadsOther", Client: memberClient, Target: targetUser.ID, WantCode: http.StatusNotFound},
		}

		for _, tc := range cases {
			t.Run(tc.Name, func(t *testing.T) {
				t.Parallel()
				ctx := testutil.Context(t, testutil.WaitLong)

				_, err := tc.Client.UserAICostSummary(ctx, tc.Target, codersdk.AIBridgeCostFilter{})
				if tc.WantCode == http.StatusOK {
					require.NoError(t, err)
					return
				}
				var sdkErr *codersdk.Error
				require.ErrorAs(t, err, &sdkErr)
				require.Equal(t, tc.WantCode, sdkErr.StatusCode())
			})
		}
	})
}

func TestAIBridgeCostUsers(t *testing.T) {
	t.Parallel()

	t.Run("RequiresLicenseFeature", func(t *testing.T) {
		t.Parallel()

		dv := coderdtest.DeploymentValues(t)
		dv.Experiments = []string{string(codersdk.ExperimentAIGatewayCostControl)}
		client, _ := coderdenttest.New(t, &coderdenttest.Options{
			Options:        &coderdtest.Options{DeploymentValues: dv},
			LicenseOptions: &coderdenttest.LicenseOptions{Features: license.Features{}},
		})
		ctx := testutil.Context(t, testutil.WaitLong)

		//nolint:gocritic // Owner role is irrelevant here; the request is blocked before RBAC.
		_, err := client.AIBridgeCostUsers(ctx, codersdk.AIBridgeCostUsersFilter{})
		var sdkErr *codersdk.Error
		require.ErrorAs(t, err, &sdkErr)
		require.Equal(t, http.StatusForbidden, sdkErr.StatusCode())
	})

	t.Run("MemberForbidden", func(t *testing.T) {
		t.Parallel()
		ownerClient, owner := coderdenttest.New(t, aiCostOpts(t))
		memberClient, _ := coderdtest.CreateAnotherUser(t, ownerClient, owner.OrganizationID)
		ctx := testutil.Context(t, testutil.WaitLong)

		_, err := memberClient.AIBridgeCostUsers(ctx, codersdk.AIBridgeCostUsersFilter{})
		var sdkErr *codersdk.Error
		require.ErrorAs(t, err, &sdkErr)
		require.Equal(t, http.StatusForbidden, sdkErr.StatusCode())
	})

	t.Run("OK", func(t *testing.T) {
		t.Parallel()
		client, db, firstUser := coderdenttest.NewWithDatabase(t, aiCostOpts(t))
		ctx := testutil.Context(t, testutil.WaitLong)

		now := dbtime.Now()
		seedCostData(t, db, firstUser.OrganizationID, firstUser.UserID, now)

		//nolint:gocritic // The endpoint requires interception read, which members lack.
		got, err := client.AIBridgeCostUsers(ctx, codersdk.AIBridgeCostUsersFilter{
			AIBridgeCostFilter: codersdk.AIBridgeCostFilter{
				StartDate: now.Add(-time.Hour),
				EndDate:   now.Add(time.Hour),
			},
		})
		require.NoError(t, err)
		require.EqualValues(t, 1, got.Count)
		require.Len(t, got.Users, 1)
		require.Equal(t, firstUser.UserID, got.Users[0].UserID)
		require.EqualValues(t, 1_500, got.Users[0].TotalCostMicros)
		require.EqualValues(t, 2, got.Users[0].RequestCount)
		require.EqualValues(t, 1, got.Users[0].UnpricedRequestCount)
		require.EqualValues(t, 2, got.Users[0].SessionCount)
		require.EqualValues(t, 300, got.Users[0].TotalInputTokens)
	})

	t.Run("RejectsAfterID", func(t *testing.T) {
		t.Parallel()
		client, _ := coderdenttest.New(t, aiCostOpts(t))
		ctx := testutil.Context(t, testutil.WaitLong)

		//nolint:gocritic // Validation fires before RBAC.
		_, err := client.AIBridgeCostUsers(ctx, codersdk.AIBridgeCostUsersFilter{
			Pagination: codersdk.Pagination{AfterID: uuid.New()},
		})
		var sdkErr *codersdk.Error
		require.ErrorAs(t, err, &sdkErr)
		require.Equal(t, http.StatusBadRequest, sdkErr.StatusCode())
	})

	t.Run("SearchNoMatch", func(t *testing.T) {
		t.Parallel()
		client, db, firstUser := coderdenttest.NewWithDatabase(t, aiCostOpts(t))
		ctx := testutil.Context(t, testutil.WaitLong)

		now := dbtime.Now()
		seedCostData(t, db, firstUser.OrganizationID, firstUser.UserID, now)

		//nolint:gocritic // The endpoint requires interception read, which members lack.
		got, err := client.AIBridgeCostUsers(ctx, codersdk.AIBridgeCostUsersFilter{
			AIBridgeCostFilter: codersdk.AIBridgeCostFilter{
				StartDate: now.Add(-time.Hour),
				EndDate:   now.Add(time.Hour),
			},
			Search: "no-such-user",
		})
		require.NoError(t, err)
		require.EqualValues(t, 0, got.Count)
		require.Empty(t, got.Users)
	})
}
