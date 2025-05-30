from braintree import (
    BraintreeGateway,
    AddOnGateway,
    Configuration,
    )
from braintree.attribute_getter import AttributeGetter
from datetime import datetime, timedelta, UTC

CONVENIENCE_OBJECTS = [
    'gateway'
]

HEADERS = {
    "X-ApiVersion": "6",
    "Accept": "application/xml",
}

# Searches return at most 10,000 results (50,000 for transaction searches). If we hit this limit,
# the connector could have missed data and we'll need to use smaller date windows.
SEARCH_LIMIT = 10_000
TRANSACTION_SEARCH_LIMIT = 50_000

SEARCH_PAGE_SIZE = 50
SEMAPHORE_LIMIT = 20


def braintree_object_to_dict(braintree_object):
        """
        Recursively convert a Braintree object and its nested objects to a dictionary.
        Convenience objects intended to make subsequent Braintree requests easier are ommitted.
        """
        if isinstance(braintree_object, (Configuration, AddOnGateway, BraintreeGateway)):
            return None
        data = braintree_object.__dict__.copy()
        # Remove convenience objects (like BraintreeGateways for making more requests).
        for key in CONVENIENCE_OBJECTS:
            data.pop(key, None)

        for key, value in data.items():
            if isinstance(value, AttributeGetter):
                data[key] = braintree_object_to_dict(value)
            elif isinstance(value, datetime):
                data[key] =  value.replace(tzinfo=UTC)
            elif hasattr(value, "__dict__"):
                data[key] = braintree_object_to_dict(value)
            elif isinstance(value, list):
                new_value = []
                for item in value:
                    if hasattr(item, "__dict__"):
                        new_value.append(braintree_object_to_dict(item))
                    elif isinstance(item, datetime):
                        new_value.append(item.replace(tzinfo=UTC))
                    else:
                        new_value.append(item)

                data[key] = new_value

        # Remove private attributes.
        data.pop('_setattrs', None)
        return data


def reduce_window_end(
    start: datetime,
    end: datetime,
) -> datetime:
    window_size = (end - start) / 2

    # Braintree's datetimes have a resolution of seconds, so we remove microseconds from the window size.
    reduced_window_size = window_size - timedelta(microseconds=window_size.microseconds)

    # It's unlikely a user will have enough data in Braintree that the connector will reduce the window size below 1 second,
    # but if it does happen the connector should raise an error since that stream will be stuck.
    if reduced_window_size < timedelta(seconds=1):
        raise RuntimeError("Window size is smaller than Braintree's datetime resolution of 1 second. Contact Estuary support for help addressing this error.")

    return start + reduced_window_size


def search_limit_error_message(count: int) -> str:
    msg = (
        f"{count} returned in a single search which is "
        f"greater than or equal to Braintree's documented maximum for a single search. "
        "Reduce the window size and backfill this stream."
    )

    return msg
