import { TriangleAlertIcon } from "lucide-react";
import { type FC, useState } from "react";
import { getErrorMessage } from "#/api/errors";
import type * as TypesGen from "#/api/typesGenerated";
import { Button } from "#/components/Button/Button";
import { PaginationWidgetBase } from "#/components/PaginationWidget/PaginationWidgetBase";
import { Spinner } from "#/components/Spinner/Spinner";
import {
	Table,
	TableBody,
	TableCell,
	TableHead,
	TableHeader,
	TableRow,
} from "#/components/Table/Table";
import {
	Tooltip,
	TooltipContent,
	TooltipTrigger,
} from "#/components/Tooltip/Tooltip";
import { formatTokenCount } from "#/utils/analytics";
import { formatCostMicros } from "#/utils/currency";
import { paginateItems } from "#/utils/paginateItems";

interface AICostSummaryViewProps {
	summary: TypesGen.AIBridgeUserCostSummary | undefined;
	isLoading: boolean;
	error: unknown;
	onRetry: () => void;
	loadingLabel: string;
	emptyMessage: string;
}

export const AICostSummaryView: FC<AICostSummaryViewProps> = ({
	summary,
	isLoading,
	error,
	onRetry,
	loadingLabel,
	emptyMessage,
}) => {
	const [modelPage, setModelPage] = useState(1);
	const [chatPage, setChatPage] = useState(1);

	if (isLoading) {
		return (
			<div
				role="status"
				aria-label={loadingLabel}
				className="flex min-h-[240px] items-center justify-center"
			>
				<Spinner size="lg" loading />
			</div>
		);
	}

	if (error != null) {
		return (
			<div className="flex min-h-[240px] flex-col items-center justify-center gap-4 text-center">
				<p className="m-0 text-sm text-content-secondary">
					{getErrorMessage(error, "Failed to load usage details.")}
				</p>
				<Button variant="outline" size="sm" type="button" onClick={onRetry}>
					Retry
				</Button>
			</div>
		);
	}

	if (!summary) {
		return null;
	}

	const modelPageSize = 10;
	const {
		pagedItems: pagedModels,
		clampedPage: clampedModelPage,
		hasPreviousPage: hasModelPrev,
		hasNextPage: hasModelNext,
	} = paginateItems(summary.by_model, modelPageSize, modelPage);
	const chatPageSize = 10;
	const {
		pagedItems: pagedChats,
		clampedPage: clampedChatPage,
		hasPreviousPage: hasChatPrev,
		hasNextPage: hasChatNext,
	} = paginateItems(summary.by_chat, chatPageSize, chatPage);

	return (
		<div className="space-y-6">
			<div className="grid grid-cols-2 gap-4 md:grid-cols-3">
				<div className="rounded-lg border border-border-default bg-surface-secondary p-4">
					<p className="text-xs font-medium uppercase tracking-wide text-content-secondary">
						Total Cost
					</p>
					<p className="mt-1 text-2xl font-semibold text-content-primary">
						{formatCostMicros(summary.total_cost_micros)}
					</p>
				</div>
				<div className="rounded-lg border border-border-default bg-surface-secondary p-4">
					<p className="text-xs font-medium uppercase tracking-wide text-content-secondary">
						Input Tokens
					</p>
					<p className="mt-1 text-2xl font-semibold text-content-primary">
						{formatTokenCount(summary.total_input_tokens)}
					</p>
				</div>
				<div className="rounded-lg border border-border-default bg-surface-secondary p-4">
					<p className="text-xs font-medium uppercase tracking-wide text-content-secondary">
						Output Tokens
					</p>
					<p className="mt-1 text-2xl font-semibold text-content-primary">
						{formatTokenCount(summary.total_output_tokens)}
					</p>
				</div>
				<div className="rounded-lg border border-border-default bg-surface-secondary p-4">
					<p className="text-xs font-medium uppercase tracking-wide text-content-secondary">
						Cache Read
					</p>
					<p className="mt-1 text-2xl font-semibold text-content-primary">
						{formatTokenCount(summary.total_cache_read_tokens)}
					</p>
				</div>
				<div className="rounded-lg border border-border-default bg-surface-secondary p-4">
					<p className="text-xs font-medium uppercase tracking-wide text-content-secondary">
						Cache Write
					</p>
					<p className="mt-1 text-2xl font-semibold text-content-primary">
						{formatTokenCount(summary.total_cache_write_tokens)}
					</p>
				</div>
				<div className="rounded-lg border border-border-default bg-surface-secondary p-4">
					<p className="text-xs font-medium uppercase tracking-wide text-content-secondary">
						Requests
					</p>
					<p className="mt-1 text-2xl font-semibold text-content-primary">
						{summary.request_count.toLocaleString("en-US")}
					</p>
				</div>
			</div>

			{summary.unpriced_request_count > 0 && (
				<div className="flex items-start gap-3 rounded-lg border border-border-warning bg-surface-warning p-4 text-sm text-content-primary">
					<TriangleAlertIcon className="size-5 shrink-0 text-content-warning" />
					<span>
						{summary.unpriced_request_count.toLocaleString("en-US")} request
						{summary.unpriced_request_count === 1 ? "" : "s"} lacked pricing
						data for some or all usage, so the total may undercount actual cost.
					</span>
				</div>
			)}

			{summary.by_model.length === 0 && summary.by_chat.length === 0 ? (
				<p className="py-12 text-center text-content-secondary">
					{emptyMessage}
				</p>
			) : (
				<>
					<div>
						<Table aria-label="Cost breakdown by model">
							<TableHeader>
								<TableRow>
									<TableHead>Provider</TableHead>
									<TableHead>Model</TableHead>
									<TableHead className="text-right">Requests</TableHead>
									<TableHead className="text-right">Cost</TableHead>
									<TableHead className="text-right">Input</TableHead>
									<TableHead className="text-right">Output</TableHead>
									<TableHead className="text-right">Cache Read</TableHead>
									<TableHead className="text-right">Cache Write</TableHead>
								</TableRow>
							</TableHeader>
							<TableBody>
								{pagedModels.map((model) => (
									<TableRow key={`${model.provider}:${model.model}`}>
										<TableCell>{model.provider}</TableCell>
										<TableCell className="text-content-secondary">
											{model.model}
										</TableCell>
										<TableCell className="text-right tabular-nums">
											{model.request_count.toLocaleString("en-US")}
										</TableCell>
										<TableCell className="text-right tabular-nums">
											{formatCostMicros(model.total_cost_micros)}
										</TableCell>
										<TableCell className="text-right tabular-nums">
											{formatTokenCount(model.total_input_tokens)}
										</TableCell>
										<TableCell className="text-right tabular-nums">
											{formatTokenCount(model.total_output_tokens)}
										</TableCell>
										<TableCell className="text-right tabular-nums">
											{formatTokenCount(model.total_cache_read_tokens)}
										</TableCell>
										<TableCell className="text-right tabular-nums">
											{formatTokenCount(model.total_cache_write_tokens)}
										</TableCell>
									</TableRow>
								))}
							</TableBody>
						</Table>
						{summary.by_model.length > modelPageSize && (
							<div className="pt-4">
								<PaginationWidgetBase
									totalRecords={summary.by_model.length}
									currentPage={clampedModelPage}
									pageSize={modelPageSize}
									onPageChange={setModelPage}
									hasPreviousPage={hasModelPrev}
									hasNextPage={hasModelNext}
								/>
							</div>
						)}
					</div>

					<div>
						<Table aria-label="Cost breakdown by chat">
							<TableHeader>
								<TableRow>
									<TableHead>Chat title</TableHead>
									<TableHead className="text-right">Requests</TableHead>
									<TableHead className="text-right">Cost</TableHead>
									<TableHead className="text-right">Input</TableHead>
									<TableHead className="text-right">Output</TableHead>
									<TableHead className="text-right">Cache Read</TableHead>
									<TableHead className="text-right">Cache Write</TableHead>
								</TableRow>
							</TableHeader>
							<TableBody>
								{pagedChats.map((chat) => {
									const chatTitle = chat.chat_title || chat.chat_id;
									return (
										<TableRow key={chat.chat_id}>
											<TableCell className="max-w-[200px]">
												<Tooltip>
													<TooltipTrigger asChild>
														<span className="block truncate">{chatTitle}</span>
													</TooltipTrigger>
													<TooltipContent>{chatTitle}</TooltipContent>
												</Tooltip>
											</TableCell>
											<TableCell className="text-right tabular-nums">
												{chat.request_count.toLocaleString("en-US")}
											</TableCell>
											<TableCell className="text-right tabular-nums">
												{formatCostMicros(chat.total_cost_micros)}
											</TableCell>
											<TableCell className="text-right tabular-nums">
												{formatTokenCount(chat.total_input_tokens)}
											</TableCell>
											<TableCell className="text-right tabular-nums">
												{formatTokenCount(chat.total_output_tokens)}
											</TableCell>
											<TableCell className="text-right tabular-nums">
												{formatTokenCount(chat.total_cache_read_tokens)}
											</TableCell>
											<TableCell className="text-right tabular-nums">
												{formatTokenCount(chat.total_cache_write_tokens)}
											</TableCell>
										</TableRow>
									);
								})}
							</TableBody>
						</Table>
						{summary.by_chat.length > chatPageSize && (
							<div className="pt-4">
								<PaginationWidgetBase
									totalRecords={summary.by_chat.length}
									currentPage={clampedChatPage}
									pageSize={chatPageSize}
									onPageChange={setChatPage}
									hasPreviousPage={hasChatPrev}
									hasNextPage={hasChatNext}
								/>
							</div>
						)}
					</div>
				</>
			)}
		</div>
	);
};
