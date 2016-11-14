package fritz

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/bpicode/fritzctl/httpread"
	"github.com/bpicode/fritzctl/logger"
	"github.com/bpicode/fritzctl/math"
	"github.com/bpicode/fritzctl/stringutils"
)

// Fritz API definition, guided by
// https://avm.de/fileadmin/user_upload/Global/Service/Schnittstellen/AHA-HTTP-Interface.pdf.
type Fritz interface {
	ListDevices() (*Devicelist, error)
	SwitchOn(names ...string) error
	SwitchOff(names ...string) error
	Toggle(names ...string) error
	Temperature(value float64, names ...string) error
}

// UsingClient is factory function to create a Fritz API interaction point.
func UsingClient(client *Client) Fritz {
	return &fritzImpl{client: client}
}

type fritzImpl struct {
	client *Client
}

func (fritz *fritzImpl) getWithAinAndParam(ain, switchcmd, param string) (*http.Response, error) {
	url := fmt.Sprintf("%s://%s:%s/%s?ain=%s&switchcmd=%s&param=%s&sid=%s",
		fritz.client.Config.Protocol,
		fritz.client.Config.Host,
		fritz.client.Config.Port,
		"/webservices/homeautoswitch.lua",
		ain,
		switchcmd,
		param,
		fritz.client.SessionInfo.SID)
	return fritz.client.HTTPClient.Get(url)
}

func (fritz *fritzImpl) getWithAin(ain, switchcmd string) (*http.Response, error) {
	url := fmt.Sprintf("%s://%s:%s/%s?ain=%s&switchcmd=%s&sid=%s",
		fritz.client.Config.Protocol,
		fritz.client.Config.Host,
		fritz.client.Config.Port,
		"/webservices/homeautoswitch.lua",
		ain,
		switchcmd,
		fritz.client.SessionInfo.SID)
	return fritz.client.HTTPClient.Get(url)
}

func (fritz *fritzImpl) get(switchcmd string) (*http.Response, error) {
	url := fmt.Sprintf("%s://%s:%s/%s?switchcmd=%s&sid=%s",
		fritz.client.Config.Protocol,
		fritz.client.Config.Host,
		fritz.client.Config.Port,
		"/webservices/homeautoswitch.lua",
		switchcmd,
		fritz.client.SessionInfo.SID)
	return fritz.client.HTTPClient.Get(url)
}

// ListDevices lists the basic data of the smart home devices.
func (fritz *fritzImpl) ListDevices() (*Devicelist, error) {
	response, errHTTP := fritz.get("getdevicelistinfos")
	if errHTTP != nil {
		return nil, errHTTP
	}
	defer response.Body.Close()
	var deviceList Devicelist
	errDecode := xml.NewDecoder(response.Body).Decode(&deviceList)
	return &deviceList, errDecode
}

// SwitchOn switches a device on. The device is identified by its name.
func (fritz *fritzImpl) SwitchOn(names ...string) error {
	return fritz.doConcurrently(func(ain string) func() (string, error) {
		return func() (string, error) {
			return fritz.switchForAin(ain, "setswitchon")
		}
	}, names...)
}

// SwitchOff switches a device off. The device is identified by its name.
func (fritz *fritzImpl) SwitchOff(names ...string) error {
	return fritz.doConcurrently(func(ain string) func() (string, error) {
		return func() (string, error) {
			return fritz.switchForAin(ain, "setswitchoff")
		}
	}, names...)
}

func (fritz *fritzImpl) switchForAin(ain, command string) (string, error) {
	resp, errSwitch := fritz.getWithAin(ain, command)
	return httpread.ReadFullyString(resp, errSwitch)
}

// Toggle toggles the on/off state of devices.
func (fritz *fritzImpl) Toggle(names ...string) error {
	return fritz.doConcurrently(func(ain string) func() (string, error) {
		return func() (string, error) {
			return fritz.toggleForAin(ain)
		}
	}, names...)
}

func (fritz *fritzImpl) toggleForAin(ain string) (string, error) {
	resp, errSwitch := fritz.getWithAin(ain, "setswitchtoggle")
	return httpread.ReadFullyString(resp, errSwitch)
}

// Temperature sets the desired temperature of "HKR" devices.
func (fritz *fritzImpl) Temperature(value float64, names ...string) error {
	return fritz.doConcurrently(func(ain string) func() (string, error) {
		return func() (string, error) {
			return fritz.temperatureForAin(ain, value)
		}
	}, names...)
}

func (fritz *fritzImpl) temperatureForAin(ain string, value float64) (string, error) {
	doubledValue := 2 * value
	rounded := math.Round(doubledValue)
	response, err := fritz.getWithAinAndParam(ain, "sethkrtsoll", fmt.Sprintf("%d", rounded))
	return httpread.ReadFullyString(response, err)
}

func (fritz *fritzImpl) doConcurrently(workFactory func(string) func() (string, error), names ...string) error {
	targets, err := buildBacklog(fritz, names, workFactory)
	if err != nil {
		return err
	}
	results := scatterGather(targets, genericSuccessHandler, genericErrorHandler)
	return genericResult(results)
}

func genericSuccessHandler(key, messsage string) result {
	logger.Success("Successfully processed device '" + key + "'; response was: " + strings.TrimSpace(messsage))
	return result{msg: messsage, err: nil}
}

func genericErrorHandler(key, message string, err error) result {
	logger.Warn("Error while processing device '" + key + "'; error was: " + err.Error())
	return result{msg: message, err: fmt.Errorf("error toggling device '%s': %s", key, err.Error())}
}

func genericResult(results []result) error {
	if err := truncateToOne(results); err != nil {
		return errors.New("Not all devices could be processed! Nested errors are: " + err.Error())
	}
	return nil
}

func truncateToOne(results []result) error {
	errs := make([]error, 0, len(results))
	for _, res := range results {
		if res.err != nil {
			errs = append(errs, res.err)
		}
	}
	if len(errs) > 0 {
		msgs := stringutils.ErrorMessages(errs)
		return errors.New(strings.Join(msgs, "; "))
	}
	return nil
}

func buildBacklog(fritz *fritzImpl, names []string, workFactory func(string) func() (string, error)) (map[string]func() (string, error), error) {
	namesAndAins, err := fritz.getNameToAinTable()
	if err != nil {
		return nil, err
	}
	targets := make(map[string]func() (string, error))
	for _, name := range names {
		ain, ok := namesAndAins[name]
		if ain == "" || !ok {
			quoted := stringutils.Quote(stringutils.StringKeys(namesAndAins))
			return nil, errors.New("No device found with name '" + name + "'. Available devices are " + strings.Join(quoted, ", "))
		}
		targets[name] = workFactory(ain)
	}
	return targets, nil
}

func (fritz *fritzImpl) getNameToAinTable() (map[string]string, error) {
	devList, err := fritz.ListDevices()
	if err != nil {
		return nil, err
	}
	devs := devList.Devices
	table := make(map[string]string, len(devs))
	for _, dev := range devs {
		table[dev.Name] = strings.Replace(dev.Identifier, " ", "", -1)
	}
	return table, nil
}
