# Bridged

## Building

Simply run `make` will build all commands, including bridged.

## Running

The prototype runs in `light mode`. The first time it is run, it will take a while for the chain to fully sync (seemingly roughly an hour).
 
An account is expected to run this example. 
- Use an existing one:
  One can be created using the default `geth` binary, and then imported. To import an account, pass the `--account-json $ACCOUNTFILE` flag. This will import the account into the oracles keystore. You must also use the `--account-pass $ACCOUNTPASS` flag with the password which was used to encrypt the account.
- Let the bridge create it's own:
  If no existing one is imported or the passed accountfile has no accounts, the bridge will create an account itself, encrypted with the passed password  from the  `--account-pass $ACCOUNTPASS` flag.

 After the first time, the account remains loaded (unless the keystore dir is removed/cleared), and only the password needs to be provided.

**Attention** The keystore is stored per network (main, rinkeby or Ropsten testnets)

### Important

If you want to create these mint transactions yourself, the provided contract will need to be deployed by the account of which you have imported the key.
The contract address can be changed [in the source](./bridge.go)