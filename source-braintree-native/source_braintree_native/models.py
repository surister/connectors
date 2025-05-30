import braintree
from braintree import BraintreeGateway
from datetime import datetime, timezone, timedelta
from logging import Logger
from pydantic import AwareDatetime, BaseModel, Field
from typing import Annotated, Any, AsyncGenerator, Callable, Literal

from estuary_cdk.capture.common import (
    BaseDocument,
    BasicAuth,
    ConnectorState as GenericConnectorState,
    LogCursor,
    PageCursor,
    ResourceConfigWithSchedule,
    ResourceState,
)
from estuary_cdk.http import HTTPSession

def default_start_date():
    dt = datetime.now(timezone.utc) - timedelta(days=30)
    return dt

class EndpointConfig(BaseModel):
    start_date: AwareDatetime = Field(
        description="UTC date and time in the format YYYY-MM-DDTHH:MM:SSZ. Any Braintree data generated before this date will not be replicated. If left blank, the start date will be set to 30 days before the present date.",
        title="Start Date",
        default_factory=default_start_date,
    )
    merchant_id: str = Field(
        description="Braintree Merchant ID associated with your account.",
        title="Merchant ID",
    )

    class ApiKey(BasicAuth):
        credentials_title: Literal["API Key"] = Field(
            default="API Key",
            json_schema_extra={"type": "string"}
        )
        username: str = Field(
            title="Public Key",
            json_schema_extra={"secret": True},
        )
        password: str = Field(
            title="Private Key",
            json_schema_extra={"secret": True},
        )

    credentials: ApiKey = Field(
        title="Authentication",
        discriminator="credentials_title"
    )

    class Advanced(BaseModel):
        is_sandbox: bool = Field(
            description="Check if you are using a Braintree Sandbox environment.",
            title="Is a Sandbox Environment",
            default=False,
        )
        window_size: Annotated[int, Field(
            description="Window size in hours for incremental streams. This should be left as the default value unless connector errors indicate a smaller window size is required.",
            title="Window Size",
            default=24,
            gt=0,
        )]

    advanced: Advanced = Field(
        default_factory=Advanced, #type: ignore
        title="Advanced Config",
        description="Advanced settings for the connector.",
        json_schema_extra={"advanced": True},
    )

ConnectorState = GenericConnectorState[ResourceState]


class FullRefreshResource(BaseDocument, extra="allow"):
    pass


class IncrementalResource(BaseDocument, extra="allow"):
    id: str
    created_at: AwareDatetime


class Transaction(IncrementalResource):
    updated_at: AwareDatetime


SnapshotFn = Callable[
    [HTTPSession, str, BraintreeGateway, Logger],
    AsyncGenerator[FullRefreshResource, None]
]


NonPaginatedSnapshotBraintreeClass = type[
    braintree.AddOn |
    braintree.Discount |
    braintree.Plan
]


IncrementalResourceBraintreeClass = type[
    braintree.CreditCardVerification |
    braintree.Customer |
    braintree.Subscription |
    braintree.Transaction
]

# Response models for undocumented APIs
class IdSearchResponse(BaseModel, extra="allow"):
    class SearchResults(BaseModel, extra="forbid"):
        page_size: int
        ids: list[str]

    search_results: SearchResults


SearchResponseResource = list[dict[str , Any]] | dict[str, Any] | None


class SearchResponseResources(BaseModel, extra="forbid"):
    current_page_number: int
    page_size: int
    total_items: int
    resource: SearchResponseResource


class SearchResponse(BaseModel, extra="allow"):
    resources: SearchResponseResources


class TransactionSearchResponse(SearchResponse):
    class CreditCardTransactions(SearchResponseResources):
        resource: SearchResponseResource = Field(alias="transaction", default=None)

    resources: CreditCardTransactions = Field(alias="credit_card_transactions")


class CreditCardVerificationSearchResponse(SearchResponse):
    class CreditCardVerifications(SearchResponseResources):
        resource: SearchResponseResource = Field(alias="verification", default=None)

    resources: CreditCardVerifications = Field(alias="credit_card_verifications")


class CustomerSearchResponse(SearchResponse):
    class Customers(SearchResponseResources):
        resource: SearchResponseResource = Field(alias="customer", default=None)

    resources: Customers = Field(alias="customers")


class SubscriptionSearchResponse(SearchResponse):
    class Subscriptions(SearchResponseResources):
        resource: SearchResponseResource = Field(alias="subscription", default=None)

    resources: Subscriptions = Field(alias="subscriptions")


class DisputesSearchResponse(SearchResponse):
    class Disputes(SearchResponseResources):
        resource: SearchResponseResource = Field(alias="dispute", default=None)

    resources: Disputes = Field(alias="disputes")


class MerchantAccountsResponse(BaseModel, extra="allow"):
    class MerchantAccounts(BaseModel, extra="forbid"):
        current_page_number: int
        page_size: int
        total_items: int
        merchant_account: list[dict[str, Any]] | dict[str, Any] | None = None

    merchant_accounts: MerchantAccounts


class NonPaginatedSnapshotResponse(BaseModel, extra="allow"):
    resources: list[dict[str, Any]] | None = None


class PlansResponse(NonPaginatedSnapshotResponse):
    resources: list[dict[str, Any]] | None = Field(alias="plans", default=None)


class DiscountsResponse(NonPaginatedSnapshotResponse):
    resources: list[dict[str, Any]] | None = Field(alias="discounts", default=None)


class AddOnsResponse(NonPaginatedSnapshotResponse):
    resources: list[dict[str, Any]] | None = Field(alias="add_ons", default=None)
