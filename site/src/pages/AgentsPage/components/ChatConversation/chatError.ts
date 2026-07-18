import type * as TypesGen from "#/api/typesGenerated";

export type ChatDetailError = {
	message: string;
	detail?: string;
	kind: TypesGen.ChatErrorKind;
	provider?: string;
	retryable?: boolean;
	statusCode?: number;
};

export const chatDetailErrorsEqual = (
	left: ChatDetailError | null | undefined,
	right: ChatDetailError | null | undefined,
): boolean => {
	if (left === right) {
		return true;
	}
	if (!left || !right) {
		return false;
	}
	return (
		left.kind === right.kind &&
		left.message === right.message &&
		left.detail === right.detail &&
		left.provider === right.provider &&
		left.retryable === right.retryable &&
		left.statusCode === right.statusCode
	);
};

export const normalizeChatErrorPayload = (
	error: TypesGen.ChatError | undefined,
): ChatDetailError | undefined => {
	const message = error?.message?.trim();
	if (!message) {
		return undefined;
	}
	const detail = error?.detail?.trim();
	const statusCode =
		typeof error?.status_code === "number" && error.status_code > 0
			? error.status_code
			: undefined;
	return {
		message,
		kind: error?.kind ?? "generic",
		provider: error?.provider?.trim() || undefined,
		retryable: error?.retryable,
		statusCode,
		...(detail ? { detail } : {}),
	};
};
