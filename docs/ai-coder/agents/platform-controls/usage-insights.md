# Spend Management

Coder provides admin-only controls for monitoring and controlling agent
spend: AI Gateway budgets and cost tracking.

## Budgets

Coder Agents spend is controlled by AI Gateway budgets, which cap all AI
Gateway usage (including Coder Agents chats) per user over the configured
budget period.

- **Group budgets**: set a budget for a group from the group's settings
  page. The deployment budget policy resolves which group budget applies
  when a user belongs to multiple budgeted groups.
- **Per-user overrides**: set a custom budget for an individual user,
  attributed to one of their groups. Takes priority over group budgets.

### Enforcement

- The AI Gateway checks the user's current spend before forwarding each
  request. When spend meets or exceeds the budget, the request is rejected
  and the chat shows a terminal error explaining that the budget was
  exceeded.
- Brief overage is possible when concurrent requests are in flight, because
  cost is recorded after the LLM responds.

### User-facing status

When a budget applies, users see their current AI spend, the budget, and
the period reset date in the usage indicator on the Agents page.

## Cost tracking

Navigate to **Agents** > **Settings** > **Manage Agents** > **Spend**.

This view shows deployment-wide AI Gateway costs with per-user drill-down.
It requires the AI Governance add-on.

### Top-level view

A per-user rollup table with the following columns:

| Column             | Description                            |
|--------------------|----------------------------------------|
| Total cost         | Aggregate dollar spend for the user    |
| Requests           | Number of intercepted LLM requests     |
| Sessions           | Number of distinct AI Gateway sessions |
| Input tokens       | Total input tokens consumed            |
| Output tokens      | Total output tokens consumed           |
| Cache read tokens  | Tokens served from cache               |
| Cache write tokens | Tokens written to cache                |

The table supports date range filtering (default: last 30 days), search by
name or username, and pagination.

### Per-user detail view

Select a user to see:

- **Summary cards**: total cost, token breakdowns, and request counts.
- **Per-model breakdown**: table of costs and token usage by provider and
  model.
- **Per-chat breakdown**: table of costs and token usage by chat.

> [!NOTE]
> Requests without pricing data for their model are counted as unpriced
> requests, and their cost is not included in totals.
