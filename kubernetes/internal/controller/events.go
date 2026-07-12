package controller

// Event reasons for BatchSandbox and Pool controllers.
const (
	// Pod lifecycle (used by both BatchSandbox and Pool controllers)
	EventReasonFailedCreate     = "FailedCreate"
	EventReasonSuccessfulCreate = "SuccessfulCreate"
	EventReasonFailedDelete     = "FailedDelete"
	EventReasonSuccessfulDelete = "SuccessfulDelete"

	// Pool allocation — recorded on BatchSandbox by pool-controller
	EventReasonScheduled = "Scheduled"

	// Pool assignment — recorded on BatchSandbox by batchsandbox-controller
	EventReasonPoolAssigned     = "PoolAssigned"
	EventReasonFailedPoolAssign = "FailedPoolAssign"

	// Pod release — recorded on BatchSandbox
	EventReasonPodReleased   = "PodReleased"
	EventReasonFailedRelease = "FailedRelease"

	// Pod eviction — recorded on Pool
	EventReasonPodEvicted = "PodEvicted"

	// Rolling update — recorded on Pool
	EventReasonPodUpdated = "PodUpdated"

	// Allocation result — recorded on Pool
	EventReasonAllocationSucceeded = "AllocationSucceeded"
	EventReasonAllocationFailed    = "AllocationFailed"

	// Pod recycle — recorded on Pool
	EventReasonPodRecycled      = "PodRecycled"
	EventReasonFailedRecyclePod = "FailedRecyclePod"
)
