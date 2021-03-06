package jail_test

import (
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	whisper "github.com/ethereum/go-ethereum/whisper/whisperv5"
	"github.com/status-im/status-go/geth"
	"github.com/status-im/status-go/geth/jail"
	"github.com/status-im/status-go/geth/params"
)

const (
	whisperMessage1  = `test message 1 (K1 -> K2, signed+encrypted, from us)`
	whisperMessage2  = `test message 2 (K1 -> K1, signed+encrypted to ourselves)`
	whisperMessage3  = `test message 3 (K1 -> "", signed broadcast)`
	whisperMessage4  = `test message 4 ("" -> "", anon broadcast)`
	whisperMessage5  = `test message 5 ("" -> K1, encrypted anon broadcast)`
	whisperMessage6  = `test message 6 (K2 -> K1, signed+encrypted, to us)`
	chatID           = "testChat"
	statusJSFilePath = "testdata/status.js"
	txSendFolder     = "testdata/tx-send/"
)

var testConfig *geth.TestConfig

func TestMain(m *testing.M) {
	// load shared test configuration
	var err error
	testConfig, err = geth.LoadTestConfig()
	if err != nil {
		panic(err)
	}

	// run tests
	retCode := m.Run()

	//time.Sleep(25 * time.Second) // to give some time to propagate txs to the rest of the network
	os.Exit(retCode)
}

func TestJailUnInited(t *testing.T) {
	errorWrapper := func(err error) string {
		return `{"error":"` + err.Error() + `"}`
	}

	expectedError := errorWrapper(jail.ErrInvalidJail)

	var jailInstance *jail.Jail
	response := jailInstance.Parse(chatID, ``)
	if response != expectedError {
		t.Errorf("error expected, but got: %v", response)
	}

	response = jailInstance.Call(chatID, `["commands", "testCommand"]`, `{"val": 12}`)
	if response != expectedError {
		t.Errorf("error expected, but got: %v", response)
	}

	_, err := jailInstance.GetVM(chatID)
	if err != jail.ErrInvalidJail {
		t.Errorf("error expected, but got: %v", err)
	}

	_, err = jailInstance.RPCClient()
	if err != jail.ErrInvalidJail {
		t.Errorf("error expected, but got: %v", err)
	}

	// now make sure that if Init is called, then Parse doesn't produce any error
	jailInstance = jail.Init(``)
	if jailInstance == nil {
		t.Error("jail instance shouldn't be nil at this point")
		return
	}
	statusJS := geth.LoadFromFile(statusJSFilePath) + `;
	_status_catalog.commands["testCommand"] = function (params) {
		return params.val * params.val;
	};`
	response = jailInstance.Parse(chatID, statusJS)
	expectedResponse := `{"result": {"commands":{},"responses":{}}}`
	if response != expectedResponse {
		t.Errorf("unexpected response received: %v", response)
	}

	// however, we still expect issue voiced if somebody tries to execute code with Call
	response = jailInstance.Call(chatID, `["commands", "testCommand"]`, `{"val": 12}`)
	if response != errorWrapper(geth.ErrInvalidGethNode) {
		t.Errorf("error expected, but got: %v", response)
	}

	// make sure that Call() succeeds when node is started
	err = geth.PrepareTestNode()
	if err != nil {
		t.Error(err)
		return
	}
	response = jailInstance.Call(chatID, `["commands", "testCommand"]`, `{"val": 12}`)
	expectedResponse = `{"result": 144}`
	if response != expectedResponse {
		t.Errorf("expected response is not returned: expected %s, got %s", expectedResponse, response)
		return
	}
}

func TestJailInit(t *testing.T) {
	err := geth.PrepareTestNode()
	if err != nil {
		t.Error(err)
		return
	}

	initCode := `
	var _status_catalog = {
		foo: 'bar'
	};
	`
	jailInstance := jail.Init(initCode)

	extraCode := `
	var extraFunc = function (x) {
	  return x * x;
	};
	`
	response := jailInstance.Parse("newChat", extraCode)

	expectedResponse := `{"result": {"foo":"bar"}}`

	if !reflect.DeepEqual(expectedResponse, response) {
		t.Error("Expected output not returned from jail.Parse()")
		return
	}
}

func TestJailFunctionCall(t *testing.T) {
	err := geth.PrepareTestNode()
	if err != nil {
		t.Error(err)
		return
	}

	jailInstance := jail.Init("")

	// load Status JS and add test command to it
	statusJS := geth.LoadFromFile(statusJSFilePath) + `;
	_status_catalog.commands["testCommand"] = function (params) {
		return params.val * params.val;
	};`
	jailInstance.Parse(chatID, statusJS)

	// call with wrong chat id
	response := jailInstance.Call("chatIdNonExistent", "", "")
	expectedError := `{"error":"Cell[chatIdNonExistent] doesn't exist."}`
	if response != expectedError {
		t.Errorf("expected error is not returned: expected %s, got %s", expectedError, response)
		return
	}

	// call extraFunc()
	response = jailInstance.Call(chatID, `["commands", "testCommand"]`, `{"val": 12}`)
	expectedResponse := `{"result": 144}`
	if response != expectedResponse {
		t.Errorf("expected response is not returned: expected %s, got %s", expectedResponse, response)
		return
	}
}

func TestJailRPCSend(t *testing.T) {
	err := geth.PrepareTestNode()
	if err != nil {
		t.Error(err)
		return
	}

	jailInstance := jail.Init("")

	// load Status JS and add test command to it
	statusJS := geth.LoadFromFile(statusJSFilePath)
	jailInstance.Parse(chatID, statusJS)

	// obtain VM for a given chat (to send custom JS to jailed version of Send())
	vm, err := jailInstance.GetVM(chatID)
	if err != nil {
		t.Errorf("cannot get VM: %v", err)
		return
	}

	// internally (since we replaced `web3.send` with `jail.Send`)
	// all requests to web3 are forwarded to `jail.Send`
	_, err = vm.Run(`
	    var balance = web3.eth.getBalance("` + testConfig.Account1.Address + `");
		var sendResult = web3.fromWei(balance, "ether")
	`)
	if err != nil {
		t.Errorf("cannot run custom code on VM: %v", err)
		return
	}

	value, err := vm.Get("sendResult")
	if err != nil {
		t.Errorf("cannot obtain result of balance check operation: %v", err)
		return
	}

	balance, err := value.ToFloat()
	if err != nil {
		t.Errorf("cannot obtain result of balance check operation: %v", err)
		return
	}

	if balance < 100 {
		t.Error("wrong balance (there should be lots of test Ether on that account)")
		return
	}

	t.Logf("Balance of %.2f ETH found on '%s' account", balance, testConfig.Account1.Address)
}

func TestJailSendQueuedTransaction(t *testing.T) {
	err := geth.PrepareTestNode()
	if err != nil {
		t.Error(err)
		return
	}

	// log into account from which transactions will be sent
	if err := geth.SelectAccount(testConfig.Account1.Address, testConfig.Account1.Password); err != nil {
		t.Errorf("cannot select account: %v", testConfig.Account1.Address)
		return
	}

	txParams := `{
  		"from": "` + testConfig.Account1.Address + `",
  		"to": "0xf82da7547534045b4e00442bc89e16186cf8c272",
  		"value": "0.000001"
	}`

	txCompletedSuccessfully := make(chan struct{})
	txCompletedCounter := make(chan struct{})
	txHashes := make(chan common.Hash)

	// replace transaction notification handler
	requireMessageId := false
	geth.SetDefaultNodeNotificationHandler(func(jsonEvent string) {
		var envelope geth.SignalEnvelope
		if err := json.Unmarshal([]byte(jsonEvent), &envelope); err != nil {
			t.Errorf("cannot unmarshal event's JSON: %s", jsonEvent)
			return
		}
		if envelope.Type == geth.EventTransactionQueued {
			event := envelope.Event.(map[string]interface{})
			messageId, ok := event["message_id"].(string)
			if !ok {
				t.Error("Message id is required, but not found")
				return
			}
			if requireMessageId {
				if len(messageId) == 0 {
					t.Error("Message id is required, but not provided")
					return
				}
			} else {
				if len(messageId) != 0 {
					t.Error("Message id is not required, but provided")
					return
				}
			}
			t.Logf("Transaction queued (will be completed shortly): {id: %s}\n", event["id"].(string))

			var txHash common.Hash
			if txHash, err = geth.CompleteTransaction(event["id"].(string), testConfig.Account1.Password); err != nil {
				t.Errorf("cannot complete queued transation[%v]: %v", event["id"], err)
			} else {
				t.Logf("Transaction complete: https://testnet.etherscan.io/tx/%s", txHash.Hex())
			}

			txCompletedSuccessfully <- struct{}{} // so that timeout is aborted
			txHashes <- txHash
			txCompletedCounter <- struct{}{}
		}
	})

	type testCommand struct {
		command          string
		params           string
		expectedResponse string
	}
	type testCase struct {
		name             string
		file             string
		requireMessageId bool
		commands         []testCommand
	}

	tests := []testCase{
		{
			// no context or message id
			name:             "Case 1: no message id or context in inited JS",
			file:             "no-message-id-or-context.js",
			requireMessageId: false,
			commands: []testCommand{
				{
					`["commands", "send"]`,
					txParams,
					`{"result": {"transaction-hash":"TX_HASH"}}`,
				},
				{
					`["commands", "getBalance"]`,
					`{"address": "` + testConfig.Account1.Address + `"}`,
					`{"result": {"balance":42}}`,
				},
			},
		},
		{
			// context is present in inited JS (but no message id is there)
			name:             "Case 2: context is present in inited JS (but no message id is there)",
			file:             "context-no-message-id.js",
			requireMessageId: false,
			commands: []testCommand{
				{
					`["commands", "send"]`,
					txParams,
					`{"result": {"context":{"` + geth.SendTransactionRequest + `":true},"result":{"transaction-hash":"TX_HASH"}}}`,
				},
				{
					`["commands", "getBalance"]`,
					`{"address": "` + testConfig.Account1.Address + `"}`,
					`{"result": {"context":{},"result":{"balance":42}}}`, // note emtpy (but present) context!
				},
			},
		},
		{
			// message id is present in inited JS, but no context is there
			name:             "Case 3: message id is present, context is not present",
			file:             "message-id-no-context.js",
			requireMessageId: true,
			commands: []testCommand{
				{
					`["commands", "send"]`,
					txParams,
					`{"result": {"transaction-hash":"TX_HASH"}}`,
				},
				{
					`["commands", "getBalance"]`,
					`{"address": "` + testConfig.Account1.Address + `"}`,
					`{"result": {"balance":42}}`, // note emtpy context!
				},
			},
		},
		{
			// both message id and context are present in inited JS (this UC is what we normally expect to see)
			name:             "Case 4: both message id and context are present",
			file:             "tx-send.js",
			requireMessageId: true,
			commands: []testCommand{
				{
					`["commands", "send"]`,
					txParams,
					`{"result": {"context":{"eth_sendTransaction":true,"message_id":"foobar"},"result":{"transaction-hash":"TX_HASH"}}}`,
				},
				{
					`["commands", "getBalance"]`,
					`{"address": "` + testConfig.Account1.Address + `"}`,
					`{"result": {"context":{"message_id":"42"},"result":{"balance":42}}}`, // message id in context, but default one is used!
				},
			},
		},
	}

	for _, test := range tests {
		jailInstance := jail.Init(geth.LoadFromFile(txSendFolder + test.file))
		geth.PanicAfter(60*time.Second, txCompletedSuccessfully, test.name)
		jailInstance.Parse(chatID, ``)

		requireMessageId = test.requireMessageId

		for _, command := range test.commands {
			go func(jail *jail.Jail, test testCase, command testCommand) {
				t.Logf("->%s: %s", test.name, command.command)
				response := jail.Call(chatID, command.command, command.params)
				var txHash common.Hash
				if command.command == `["commands", "send"]` {
					txHash = <-txHashes
				}
				expectedResponse := strings.Replace(command.expectedResponse, "TX_HASH", txHash.Hex(), 1)
				if response != expectedResponse {
					t.Errorf("expected response is not returned: expected %s, got %s", expectedResponse, response)
					return
				}
			}(jailInstance, test, command)
		}
		<-txCompletedCounter
	}
}

func TestJailMultipleInitSingletonJail(t *testing.T) {
	err := geth.PrepareTestNode()
	if err != nil {
		t.Error(err)
		return
	}

	jailInstance1 := jail.Init("")
	jailInstance2 := jail.Init("")
	jailInstance3 := jail.New()
	jailInstance4 := jail.GetInstance()

	if !reflect.DeepEqual(jailInstance1, jailInstance2) {
		t.Error("singleton property of jail instance is violated")
	}
	if !reflect.DeepEqual(jailInstance2, jailInstance3) {
		t.Error("singleton property of jail instance is violated")
	}
	if !reflect.DeepEqual(jailInstance3, jailInstance4) {
		t.Error("singleton property of jail instance is violated")
	}
}

func TestJailGetVM(t *testing.T) {
	err := geth.PrepareTestNode()
	if err != nil {
		t.Error(err)
		return
	}

	jailInstance := jail.Init("")

	expectedError := `Cell[nonExistentChat] doesn't exist.`
	_, err = jailInstance.GetVM("nonExistentChat")
	if err == nil || err.Error() != expectedError {
		t.Error("expected error, but call succeeded")
	}

	// now let's create VM..
	jailInstance.Parse(chatID, ``)
	// ..and see if VM becomes available
	_, err = jailInstance.GetVM(chatID)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestIsConnected(t *testing.T) {
	err := geth.PrepareTestNode()
	if err != nil {
		t.Error(err)
		return
	}

	jailInstance := jail.Init("")
	jailInstance.Parse(chatID, "")

	// obtain VM for a given chat (to send custom JS to jailed version of Send())
	vm, err := jailInstance.GetVM(chatID)
	if err != nil {
		t.Errorf("cannot get VM: %v", err)
		return
	}

	_, err = vm.Run(`
	    var responseValue = web3.isConnected();
	    responseValue = JSON.stringify(responseValue);
	`)
	if err != nil {
		t.Errorf("cannot run custom code on VM: %v", err)
		return
	}

	responseValue, err := vm.Get("responseValue")
	if err != nil {
		t.Errorf("cannot obtain result of isConnected(): %v", err)
		return
	}

	response, err := responseValue.ToString()
	if err != nil {
		t.Errorf("cannot parse result: %v", err)
		return
	}

	expectedResponse := `{"jsonrpc":"2.0","result":true}`
	if !reflect.DeepEqual(response, expectedResponse) {
		t.Errorf("expected response is not returned: expected %s, got %s", expectedResponse, response)
		return
	}
}

func TestLocalStorageSet(t *testing.T) {
	err := geth.PrepareTestNode()
	if err != nil {
		t.Error(err)
		return
	}

	jailInstance := jail.Init("")
	jailInstance.Parse(chatID, "")

	// obtain VM for a given chat (to send custom JS to jailed version of Send())
	vm, err := jailInstance.GetVM(chatID)
	if err != nil {
		t.Errorf("cannot get VM: %v", err)
		return
	}

	testData := "foobar"

	opCompletedSuccessfully := make(chan struct{}, 1)

	// replace transaction notification handler
	geth.SetDefaultNodeNotificationHandler(func(jsonEvent string) {
		var envelope geth.SignalEnvelope
		if err := json.Unmarshal([]byte(jsonEvent), &envelope); err != nil {
			t.Errorf("cannot unmarshal event's JSON: %s", jsonEvent)
			return
		}
		if envelope.Type == jail.EventLocalStorageSet {
			event := envelope.Event.(map[string]interface{})
			chatId, ok := event["chat_id"].(string)
			if !ok {
				t.Error("Chat id is required, but not found")
				return
			}
			if chatId != chatID {
				t.Errorf("incorrect chat id: expected %q, got: %q", chatID, chatId)
				return
			}

			actualData, ok := event["data"].(string)
			if !ok {
				t.Error("Data field is required, but not found")
				return
			}

			if actualData != testData {
				t.Errorf("incorrect data: expected %q, got: %q", testData, actualData)
				return
			}

			t.Logf("event processed: %s", jsonEvent)
			opCompletedSuccessfully <- struct{}{} // so that timeout is aborted
		}
	})

	_, err = vm.Run(`
	    var responseValue = localStorage.set("` + testData + `");
	    responseValue = JSON.stringify(responseValue);
	`)
	if err != nil {
		t.Errorf("cannot run custom code on VM: %v", err)
		return
	}

	// make sure that signal is sent (and its parameters are correct)
	select {
	case <-opCompletedSuccessfully:
		// pass
	case <-time.After(3 * time.Second):
		t.Error("operation timed out")
	}

	responseValue, err := vm.Get("responseValue")
	if err != nil {
		t.Errorf("cannot obtain result of localStorage.set(): %v", err)
		return
	}

	response, err := responseValue.ToString()
	if err != nil {
		t.Errorf("cannot parse result: %v", err)
		return
	}

	expectedResponse := `{"jsonrpc":"2.0","result":true}`
	if !reflect.DeepEqual(response, expectedResponse) {
		t.Errorf("expected response is not returned: expected %s, got %s", expectedResponse, response)
		return
	}
}

func TestContractDeployment(t *testing.T) {
	err := geth.PrepareTestNode()
	if err != nil {
		t.Error(err)
		return
	}

	jailInstance := jail.Init("")
	jailInstance.Parse(chatID, "")

	// obtain VM for a given chat (to send custom JS to jailed version of Send())
	vm, err := jailInstance.GetVM(chatID)
	if err != nil {
		t.Errorf("cannot get VM: %v", err)
		return
	}

	// make sure you panic if transaction complete doesn't return
	completeQueuedTransaction := make(chan struct{}, 1)
	geth.PanicAfter(30*time.Second, completeQueuedTransaction, "TestContractDeployment")

	// replace transaction notification handler
	var txHash common.Hash
	geth.SetDefaultNodeNotificationHandler(func(jsonEvent string) {
		var envelope geth.SignalEnvelope
		if err := json.Unmarshal([]byte(jsonEvent), &envelope); err != nil {
			t.Errorf("cannot unmarshal event's JSON: %s", jsonEvent)
			return
		}
		if envelope.Type == geth.EventTransactionQueued {
			event := envelope.Event.(map[string]interface{})

			t.Logf("Transaction queued (will be completed shortly): {id: %s}\n", event["id"].(string))

			if err := geth.SelectAccount(testConfig.Account1.Address, testConfig.Account1.Password); err != nil {
				t.Errorf("cannot select account: %v", testConfig.Account1.Address)
				return
			}

			if txHash, err = geth.CompleteTransaction(event["id"].(string), testConfig.Account1.Password); err != nil {
				t.Errorf("cannot complete queued transation[%v]: %v", event["id"], err)
				return
			} else {
				t.Logf("Contract created: https://testnet.etherscan.io/tx/%s", txHash.Hex())
			}

			close(completeQueuedTransaction) // so that timeout is aborted
		}
	})

	_, err = vm.Run(`
		var responseValue = null;
		var testContract = web3.eth.contract([{"constant":true,"inputs":[{"name":"a","type":"int256"}],"name":"double","outputs":[{"name":"","type":"int256"}],"payable":false,"type":"function"}]);
		var test = testContract.new(
		{
			from: '` + testConfig.Account1.Address + `',
			data: '0x6060604052341561000c57fe5b5b60a58061001b6000396000f30060606040526000357c0100000000000000000000000000000000000000000000000000000000900463ffffffff1680636ffa1caa14603a575bfe5b3415604157fe5b60556004808035906020019091905050606b565b6040518082815260200191505060405180910390f35b60008160020290505b9190505600a165627a7a72305820ccdadd737e4ac7039963b54cee5e5afb25fa859a275252bdcf06f653155228210029',
			gas: '` + strconv.Itoa(params.DefaultGas) + `'
		}, function (e, contract){
			if (!e) {
				responseValue = contract.transactionHash
			}
		})
	`)
	if err != nil {
		t.Errorf("cannot run custom code on VM: %v", err)
		return
	}

	<-completeQueuedTransaction

	responseValue, err := vm.Get("responseValue")
	if err != nil {
		t.Errorf("vm.Get() failed: %v", err)
		return
	}

	response, err := responseValue.ToString()
	if err != nil {
		t.Errorf("cannot parse result: %v", err)
		return
	}

	expectedResponse := txHash.Hex()
	if !reflect.DeepEqual(response, expectedResponse) {
		t.Errorf("expected response is not returned: expected %s, got %s", expectedResponse, response)
		return
	}
}

func TestGasEstimation(t *testing.T) {
	err := geth.PrepareTestNode()
	if err != nil {
		t.Error(err)
		return
	}

	jailInstance := jail.Init("")
	jailInstance.Parse(chatID, "")

	// obtain VM for a given chat (to send custom JS to jailed version of Send())
	vm, err := jailInstance.GetVM(chatID)
	if err != nil {
		t.Errorf("cannot get VM: %v", err)
		return
	}

	// make sure you panic if transaction complete doesn't return
	completeQueuedTransaction := make(chan struct{}, 1)
	geth.PanicAfter(30*time.Second, completeQueuedTransaction, "TestContractDeployment")

	// replace transaction notification handler
	var txHash common.Hash
	geth.SetDefaultNodeNotificationHandler(func(jsonEvent string) {
		var envelope geth.SignalEnvelope
		if err := json.Unmarshal([]byte(jsonEvent), &envelope); err != nil {
			t.Errorf("cannot unmarshal event's JSON: %s", jsonEvent)
			return
		}
		if envelope.Type == geth.EventTransactionQueued {
			event := envelope.Event.(map[string]interface{})

			t.Logf("Transaction queued (will be completed immediately): {id: %s}\n", event["id"].(string))

			if err := geth.SelectAccount(testConfig.Account1.Address, testConfig.Account1.Password); err != nil {
				t.Errorf("cannot select account: %v", testConfig.Account1.Address)
				return
			}

			if txHash, err = geth.CompleteTransaction(event["id"].(string), testConfig.Account1.Password); err != nil {
				t.Errorf("cannot complete queued transation[%v]: %v", event["id"], err)
				return
			} else {
				t.Logf("Contract created: https://testnet.etherscan.io/tx/%s", txHash.Hex())
			}

			close(completeQueuedTransaction) // so that timeout is aborted
		}
	})

	_, err = vm.Run(`
		var responseValue = null;
		var testContract = web3.eth.contract([{"constant":true,"inputs":[{"name":"a","type":"int256"}],"name":"double","outputs":[{"name":"","type":"int256"}],"payable":false,"type":"function"}]);
		var test = testContract.new(
		{
			from: '` + testConfig.Account1.Address + `',
			data: '0x6060604052341561000c57fe5b5b60a58061001b6000396000f30060606040526000357c0100000000000000000000000000000000000000000000000000000000900463ffffffff1680636ffa1caa14603a575bfe5b3415604157fe5b60556004808035906020019091905050606b565b6040518082815260200191505060405180910390f35b60008160020290505b9190505600a165627a7a72305820ccdadd737e4ac7039963b54cee5e5afb25fa859a275252bdcf06f653155228210029',
		}, function (e, contract){
			if (!e) {
				responseValue = contract.transactionHash
			}
		})
	`)
	if err != nil {
		t.Errorf("cannot run custom code on VM: %v", err)
		return
	}

	<-completeQueuedTransaction

	responseValue, err := vm.Get("responseValue")
	if err != nil {
		t.Errorf("vm.Get() failed: %v", err)
		return
	}

	response, err := responseValue.ToString()
	if err != nil {
		t.Errorf("cannot parse result: %v", err)
		return
	}

	expectedResponse := txHash.Hex()
	if !reflect.DeepEqual(response, expectedResponse) {
		t.Errorf("expected response is not returned: expected %s, got %s", expectedResponse, response)
		return
	}
}

func TestJailWhisper(t *testing.T) {
	err := geth.PrepareTestNode()
	if err != nil {
		t.Error(err)
		return
	}

	whisperService, err := geth.NodeManagerInstance().WhisperService()
	if err != nil {
		t.Errorf("whisper service not running: %v", err)
	}
	whisperAPI := whisper.NewPublicWhisperAPI(whisperService)

	// account1
	_, accountKey1, err := geth.AddressToDecryptedAccount(testConfig.Account1.Address, testConfig.Account1.Password)
	if err != nil {
		t.Fatal(err)
	}
	accountKey1Hex := common.ToHex(crypto.FromECDSAPub(&accountKey1.PrivateKey.PublicKey))

	whisperService.AddIdentity(accountKey1.PrivateKey)
	if ok, err := whisperAPI.HasIdentity(accountKey1Hex); err != nil || !ok {
		t.Fatalf("identity not injected: %v", accountKey1Hex)
	}

	// account2
	_, accountKey2, err := geth.AddressToDecryptedAccount(testConfig.Account2.Address, testConfig.Account2.Password)
	if err != nil {
		t.Fatal(err)
	}
	accountKey2Hex := common.ToHex(crypto.FromECDSAPub(&accountKey2.PrivateKey.PublicKey))

	whisperService.AddIdentity(accountKey2.PrivateKey)
	if ok, err := whisperAPI.HasIdentity(accountKey2Hex); err != nil || !ok {
		t.Fatalf("identity not injected: %v", accountKey2Hex)
	}

	passedTests := map[string]bool{
		whisperMessage1: false,
		whisperMessage2: false,
		whisperMessage3: false,
		whisperMessage4: false,
		whisperMessage5: false,
		whisperMessage6: false,
	}
	installedFilters := map[string]string{
		whisperMessage1: "",
		whisperMessage2: "",
		whisperMessage3: "",
		whisperMessage4: "",
		whisperMessage5: "",
		whisperMessage6: "",
	}

	jailInstance := jail.Init("")

	testCases := []struct {
		name      string
		testCode  string
		useFilter bool
	}{
		{
			"test 0: ensure correct version of Whisper is used",
			`
				var expectedVersion = '0x5';
				if (web3.version.whisper != expectedVersion) {
					throw 'unexpected shh version, expected: ' + expectedVersion + ', got: ' + web3.version.whisper;
				}
			`,
			false,
		},
		{
			"test 1: encrypted signed message from us (From != nil && To != nil)",
			`
				var identity1 = '` + accountKey1Hex + `';
				if (!web3.shh.hasIdentity(identity1)) {
					throw 'idenitity "` + accountKey1Hex + `" not found in whisper';
				}

				var identity2 = '` + accountKey2Hex + `';
				if (!web3.shh.hasIdentity(identity2)) {
					throw 'idenitity "` + accountKey2Hex + `" not found in whisper';
				}

				var topic = 'example1';
				var payload = '` + whisperMessage1 + `';

				// start watching for messages
				var filter = shh.filter({
					from: identity1,
					to: identity2,
					topics: [web3.fromAscii(topic)]
				});

				// post message
				var message = {
				  from: identity1,
				  to: identity2,
				  topics: [web3.fromAscii(topic)],
				  payload: payload,
				  ttl: 20,
				};
				var err = shh.post(message)
				if (err !== null) {
					throw 'message not sent: ' + message;
				}

				var filterName = '` + whisperMessage1 + `';
				var filterId = filter.filterId;
				if (!filterId) {
					throw 'filter not installed properly';
				}
			`,
			true,
		},
		{
			"test 2: encrypted signed message to yourself (From != nil && To != nil)",
			`
				var identity = '` + accountKey1Hex + `';
				if (!web3.shh.hasIdentity(identity)) {
					throw 'idenitity "` + accountKey1Hex + `" not found in whisper';
				}

				var topic = 'example2';
				var payload = '` + whisperMessage2 + `';

				// start watching for messages
				var filter = shh.filter({
					from: identity,
					to: identity,
					topics: [web3.fromAscii(topic)],
				});

				// post message
				var message = {
				  from: identity,
				  to: identity,
				  topics: [web3.fromAscii(topic)],
				  payload: payload,
				  ttl: 20,
				};
				var err = shh.post(message)
				if (err !== null) {
					throw 'message not sent: ' + message;
				}

				var filterName = '` + whisperMessage2 + `';
				var filterId = filter.filterId;
				if (!filterId) {
					throw 'filter not installed properly';
				}
			`,
			true,
		},
		{
			"test 3: signed (known sender) broadcast (From != nil && To == nil)",
			`
				var identity = '` + accountKey1Hex + `';
				if (!web3.shh.hasIdentity(identity)) {
					throw 'idenitity "` + accountKey1Hex + `" not found in whisper';
				}

				var topic = 'example3';
				var payload = '` + whisperMessage3 + `';

				// generate symmetric key (if doesn't already exist)
				if (!shh.hasSymKey(topic)) {
					shh.addSymKey(topic, "0xdeadbeef"); // alternatively: shh.generateSymKey("example3");
														// to delete key, rely on: shh.deleteSymKey(topic);
				}

				// start watching for messages
				var filter = shh.filter({
					from: identity,
					topics: [web3.fromAscii(topic)],
					keyname: topic // you can use some other name for key too
				});

				// post message
				var message = {
					from: identity,
					topics: [web3.fromAscii(topic)],
					payload: payload,
					ttl: 20,
					keyname: topic
				};
				var err = shh.post(message)
				if (err !== null) {
					throw 'message not sent: ' + message;
				}

				var filterName = '` + whisperMessage3 + `';
				var filterId = filter.filterId;
				if (!filterId) {
					throw 'filter not installed properly';
				}
			`,
			true,
		},
		{
			"test 4: anonymous broadcast (From == nil && To == nil)",
			`
				var topic = 'example4';
				var payload = '` + whisperMessage4 + `';

				// generate symmetric key (if doesn't already exist)
				if (!shh.hasSymKey(topic)) {
					shh.addSymKey(topic, "0xdeadbeef"); // alternatively: shh.generateSymKey("example3");
														// to delete key, rely on: shh.deleteSymKey(topic);
				}

				// start watching for messages
				var filter = shh.filter({
					topics: [web3.fromAscii(topic)],
					keyname: topic // you can use some other name for key too
				});

				// post message
				var message = {
					topics: [web3.fromAscii(topic)],
					payload: payload,
					ttl: 20,
					keyname: topic
				};
				var err = shh.post(message)
				if (err !== null) {
					throw 'message not sent: ' + err;
				}

				var filterName = '` + whisperMessage4 + `';
				var filterId = filter.filterId;
				if (!filterId) {
					throw 'filter not installed properly';
				}
			`,
			true,
		},
		{
			"test 5: encrypted anonymous message (From == nil && To != nil)",
			`
				var identity = '` + accountKey2Hex + `';
				if (!web3.shh.hasIdentity(identity)) {
					throw 'idenitity "` + accountKey2Hex + `" not found in whisper';
				}

				var topic = 'example5';
				var payload = '` + whisperMessage5 + `';

				// start watching for messages
				var filter = shh.filter({
					to: identity,
					topics: [web3.fromAscii(topic)],
				});

				// post message
				var message = {
					to: identity,
					topics: [web3.fromAscii(topic)],
					payload: payload,
					ttl: 20
				};
				var err = shh.post(message)
				if (err !== null) {
					throw 'message not sent: ' + message;
				}

				var filterName = '` + whisperMessage5 + `';
				var filterId = filter.filterId;
				if (!filterId) {
					throw 'filter not installed properly';
				}
			`,
			true,
		},
		{
			"test 6: encrypted signed response to us (From != nil && To != nil)",
			`
				var identity1 = '` + accountKey1Hex + `';
				if (!web3.shh.hasIdentity(identity1)) {
					throw 'idenitity "` + accountKey1Hex + `" not found in whisper';
				}

				var identity2 = '` + accountKey2Hex + `';
				if (!web3.shh.hasIdentity(identity2)) {
					throw 'idenitity "` + accountKey2Hex + `" not found in whisper';
				}

				var topic = 'example6';
				var payload = '` + whisperMessage6 + `';

				// start watching for messages
				var filter = shh.filter({
					from: identity2,
					to: identity1,
					topics: [web3.fromAscii(topic)]
				});

				// post message
				var message = {
				  from: identity2,
				  to: identity1,
				  topics: [web3.fromAscii(topic)],
				  payload: payload,
				  ttl: 20
				};
				var err = shh.post(message)
				if (err !== null) {
					throw 'message not sent: ' + message;
				}

				var filterName = '` + whisperMessage6 + `';
				var filterId = filter.filterId;
				if (!filterId) {
					throw 'filter not installed properly';
				}
			`,
			true,
		},
	}

	for _, testCase := range testCases {
		t.Log(testCase.name)
		testCaseKey := crypto.Keccak256Hash([]byte(testCase.name)).Hex()
		jailInstance.Parse(testCaseKey, `var shh = web3.shh;`)
		vm, err := jailInstance.GetVM(testCaseKey)
		if err != nil {
			t.Errorf("cannot get VM: %v", err)
			return
		}

		// post messages
		if _, err := vm.Run(testCase.testCode); err != nil {
			t.Error(err)
			return
		}

		if !testCase.useFilter {
			continue
		}

		// update installed filters
		filterId, err := vm.Get("filterId")
		if err != nil {
			t.Errorf("cannot get filterId: %v", err)
			return
		}
		filterName, err := vm.Get("filterName")
		if err != nil {
			t.Errorf("cannot get filterName: %v", err)
			return
		}

		if _, ok := installedFilters[filterName.String()]; !ok {
			t.Fatal("unrecognized filter")
		}

		installedFilters[filterName.String()] = filterId.String()
	}

	time.Sleep(2 * time.Second) // allow whisper to poll

	for testKey, filter := range installedFilters {
		if filter != "" {
			t.Logf("filter found: %v", filter)
			for _, message := range whisperAPI.GetFilterChanges(filter) {
				t.Logf("message found: %s", common.FromHex(message.Payload))
				passedTests[testKey] = true
			}
		}
	}

	for testName, passedTest := range passedTests {
		if !passedTest {
			t.Fatalf("test not passed: %v", testName)
		}
	}
}
