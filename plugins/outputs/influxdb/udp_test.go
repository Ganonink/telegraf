package influxdb_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/metric"
	"github.com/influxdata/telegraf/plugins/outputs/influxdb"
	"github.com/influxdata/telegraf/plugins/serializers/influx"
	"github.com/stretchr/testify/require"
)

var (
	metricString = "cpu value=42 0\n"
)

func getMetric() telegraf.Metric {
	metric, err := metric.New(
		"cpu",
		map[string]string{},
		map[string]interface{}{
			"value": 42.0,
		},
		time.Unix(0, 0),
	)
	if err != nil {
		panic(err)
	}
	return metric
}

func getURL() *url.URL {
	u, err := url.Parse("udp://localhost:0")
	if err != nil {
		panic(err)
	}
	return u
}

type MockConn struct {
	WriteF func(b []byte) (n int, err error)
	CloseF func() error
}

func (c *MockConn) Write(b []byte) (n int, err error) {
	return c.WriteF(b)
}

func (c *MockConn) Close() error {
	return c.CloseF()
}

type MockDialer struct {
	DialContextF func(network, address string) (influxdb.Conn, error)
}

func (d *MockDialer) DialContext(ctx context.Context, network string, address string) (influxdb.Conn, error) {
	return d.DialContextF(network, address)
}

type MockSerializer struct {
	SerializeF func(metric telegraf.Metric) ([]byte, error)
}

func (s *MockSerializer) Serialize(metric telegraf.Metric) ([]byte, error) {
	return s.SerializeF(metric)
}

func TestUDP_NewUDPClientNoURL(t *testing.T) {
	config := &influxdb.UDPConfig{}
	_, err := influxdb.NewUDPClient(config)
	require.Equal(t, err, influxdb.ErrMissingURL)
}

func TestUDP_URL(t *testing.T) {
	u := getURL()
	config := &influxdb.UDPConfig{
		URL: u,
	}

	client, err := influxdb.NewUDPClient(config)
	require.NoError(t, err)

	require.Equal(t, u.String(), client.URL())
}

func TestUDP_Simple(t *testing.T) {
	var buffer bytes.Buffer

	config := &influxdb.UDPConfig{
		URL: getURL(),
		Dialer: &MockDialer{
			DialContextF: func(network, address string) (influxdb.Conn, error) {
				conn := &MockConn{
					WriteF: func(b []byte) (n int, err error) {
						buffer.Write(b)
						return 0, nil
					},
				}
				return conn, nil
			},
		},
	}
	client, err := influxdb.NewUDPClient(config)
	require.NoError(t, err)

	ctx := context.Background()
	err = client.Write(ctx, []telegraf.Metric{
		getMetric(),
		getMetric(),
	})
	require.NoError(t, err)

	require.Equal(t, metricString+metricString, buffer.String())
}

func TestUDP_DialError(t *testing.T) {
	u, err := url.Parse("invalid://127.0.0.1:9999")
	require.NoError(t, err)

	config := &influxdb.UDPConfig{
		URL: u,
		Dialer: &MockDialer{
			DialContextF: func(network, address string) (influxdb.Conn, error) {
				return nil, fmt.Errorf(
					`unsupported scheme [invalid://localhost:9999]: "invalid"`)
			},
		},
	}
	client, err := influxdb.NewUDPClient(config)
	require.NoError(t, err)

	ctx := context.Background()
	err = client.Write(ctx, []telegraf.Metric{getMetric()})
	require.Error(t, err)
}

func TestUDP_WriteError(t *testing.T) {
	closed := false

	config := &influxdb.UDPConfig{
		URL: getURL(),
		Dialer: &MockDialer{
			DialContextF: func(network, address string) (influxdb.Conn, error) {
				conn := &MockConn{
					WriteF: func(b []byte) (n int, err error) {
						return 0, fmt.Errorf(
							"write udp 127.0.0.1:52190->127.0.0.1:9999: write: connection refused")
					},
					CloseF: func() error {
						closed = true
						return nil
					},
				}
				return conn, nil
			},
		},
	}
	client, err := influxdb.NewUDPClient(config)
	require.NoError(t, err)

	ctx := context.Background()
	err = client.Write(ctx, []telegraf.Metric{getMetric()})
	require.Error(t, err)
	require.True(t, closed)
}

func TestUDP_SerializeError(t *testing.T) {
	config := &influxdb.UDPConfig{
		URL: getURL(),
		Dialer: &MockDialer{
			DialContextF: func(network, address string) (influxdb.Conn, error) {
				conn := &MockConn{}
				return conn, nil
			},
		},
		Serializer: &MockSerializer{
			SerializeF: func(metric telegraf.Metric) ([]byte, error) {
				return nil, influx.ErrNeedMoreSpace
			},
		},
	}
	client, err := influxdb.NewUDPClient(config)
	require.NoError(t, err)

	ctx := context.Background()
	err = client.Write(ctx, []telegraf.Metric{getMetric()})
	require.Error(t, err)
	require.Contains(t, err.Error(), influx.ErrNeedMoreSpace.Error())
}

func TestUDP_WriteWithRealConn(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.0:0")
	require.NoError(t, err)

	metrics := []telegraf.Metric{
		getMetric(),
		getMetric(),
	}

	buf := make([]byte, 200)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var total int
		for _, _ = range metrics {
			n, _, err := conn.ReadFrom(buf[total:])
			if err != nil {
				break
			}
			total += n
		}
		buf = buf[:total]
	}()

	addr := conn.LocalAddr()
	u, err := url.Parse(fmt.Sprintf("%s://%s", addr.Network(), addr))
	require.NoError(t, err)

	config := &influxdb.UDPConfig{
		URL: u,
	}
	client, err := influxdb.NewUDPClient(config)
	require.NoError(t, err)

	ctx := context.Background()
	err = client.Write(ctx, metrics)
	require.NoError(t, err)

	wg.Wait()

	require.Equal(t, metricString+metricString, string(buf))
}
