send $gross_fiat (
  source = @platform:banks:$bank_id:reserve
  destination = @platform:redemptions:$redemption_id:settling
)

send $net_fiat (
  source = @platform:redemptions:$redemption_id:payable
  destination = @external:fiat:payouts
)

set_tx_meta("event_type", "redeem_settle")
set_tx_meta("redemption_id", $redemption_id)
set_tx_meta("redemption_ref", $redemption_ref); meta: fixture balances default:bank_id USD/2 1000000; default:redemption_id USD/2 1000000; external:fiat:payouts USD/2 1000000; platform:banks:default:bank_id:reserve USD/2 1000000; platform:redemptions:default:redemption_id:payable USD/2 1000000; platform:redemptions:default:redemption_id:settling USD/2 1000000; world USD/2 1000000; variables bank_id default:bank_id; gross_fiat USD/2 100; net_fiat USD/2 100; redemption_id default:redemption_id; redemption_ref redemption_ref; flags experimental-account-interpolation; interpreter experimental; parsedOk: true; runOk: true; source: file schemas-library/stablecoin-issuer.yaml; kind schema-yaml; line 194; txName REDEEM_SETTLE
- schemas-library/stablecoin-issuer.yaml#REDEEM_RETURN — code: #![feature("experimental-account-interpolation")]
vars {
  account $bank_id
  account $holder_id
  account $network_id
  account $redemption_id
  monetary $token_amount
  monetary $net_fiat
  monetary $fee
  string $original_posting_id
}

send $net_fiat (
  source = @external:fiat:payouts allowing unbounded overdraft
  destination = @platform:banks:$bank_id:reserve
)