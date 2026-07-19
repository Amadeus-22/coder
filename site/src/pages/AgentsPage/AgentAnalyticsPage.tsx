import dayjs, { type Dayjs } from "dayjs";
import { type FC, useState } from "react";
import { useQuery } from "react-query";
import { useLocation } from "react-router";
import { userAICostSummary } from "#/api/queries/aiBridge";
import { ScrollArea } from "#/components/ScrollArea/ScrollArea";
import { useAuthContext } from "#/contexts/auth/AuthProvider";
import { useDashboard } from "#/modules/dashboard/useDashboard";
import { AgentAnalyticsPageView } from "./AgentAnalyticsPageView";
import { AgentPageHeader } from "./components/AgentPageHeader";

const createDateRange = (now?: Dayjs) => {
	const end = now ?? dayjs();
	const start = end.subtract(30, "day");
	return {
		startDate: start.toISOString(),
		endDate: end.toISOString(),
		rangeLabel: `${start.format("MMM D")} – ${end.format("MMM D, YYYY")}`,
	};
};

interface AgentAnalyticsPageProps {
	/** Override the current time for deterministic storybook snapshots. */
	now?: Dayjs;
}

const AgentAnalyticsPage: FC<AgentAnalyticsPageProps> = ({ now }) => {
	const { user } = useAuthContext();
	const { entitlements, experiments } = useDashboard();
	const location = useLocation();
	const [anchor] = useState<Dayjs>(() => dayjs());
	const dateRange = createDateRange(now ?? anchor);
	// TODO(AIGOV-443): drop the experiment gate once cost control is stable.
	const isEntitled =
		(entitlements.features.aibridge.entitlement === "entitled" ||
			entitlements.features.aibridge.entitlement === "grace_period") &&
		entitlements.features.aibridge.enabled &&
		experiments.includes("ai-gateway-cost-control");

	const summaryQuery = useQuery({
		...userAICostSummary(user?.id ?? "me", {
			start_date: dateRange.startDate,
			end_date: dateRange.endDate,
			client: "Coder Agents",
		}),
		enabled: Boolean(user?.id) && isEntitled,
	});

	return (
		<ScrollArea className="min-h-0 flex-1" viewportClassName="[&>div]:!block">
			<AgentPageHeader
				mobileBack={{
					to: { pathname: "/agents", search: location.search },
					label: "Agents",
				}}
			/>
			<AgentAnalyticsPageView
				isEntitled={isEntitled}
				summary={summaryQuery.data}
				isLoading={summaryQuery.isLoading}
				error={summaryQuery.error}
				onRetry={() => void summaryQuery.refetch()}
				rangeLabel={dateRange.rangeLabel}
			/>
		</ScrollArea>
	);
};

export default AgentAnalyticsPage;
