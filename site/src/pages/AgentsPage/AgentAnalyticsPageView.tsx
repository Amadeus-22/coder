import { BarChart3Icon } from "lucide-react";
import type { FC } from "react";
import type { AIBridgeUserCostSummary } from "#/api/typesGenerated";
import { PaywallAIGovernance } from "#/components/Paywall/PaywallAIGovernance";
import { AICostSummaryView } from "#/pages/AISettingsPage/SpendPage/components/AICostSummaryView";
import { SectionHeader } from "./components/SectionHeader";

interface AgentAnalyticsPageViewProps {
	isEntitled: boolean;
	summary: AIBridgeUserCostSummary | undefined;
	isLoading: boolean;
	error: unknown;
	onRetry: () => void;
	rangeLabel: string;
}

export const AgentAnalyticsPageView: FC<AgentAnalyticsPageViewProps> = ({
	isEntitled,
	summary,
	isLoading,
	error,
	onRetry,
	rangeLabel,
}) => {
	return (
		<div className="flex flex-col p-4 pt-8">
			<div className="mx-auto w-full max-w-3xl">
				<SectionHeader
					label="Analytics"
					description="Review your personal Coder Agents usage and cost breakdowns."
					action={
						<div className="flex items-center gap-2 text-xs text-content-secondary">
							<BarChart3Icon className="size-4" />
							<span>{rangeLabel}</span>
						</div>
					}
				/>

				{isEntitled ? (
					<AICostSummaryView
						summary={summary}
						isLoading={isLoading}
						error={error}
						onRetry={onRetry}
						loadingLabel="Loading analytics"
						emptyMessage="No usage data for you in this period."
					/>
				) : (
					<PaywallAIGovernance />
				)}
			</div>
		</div>
	);
};
