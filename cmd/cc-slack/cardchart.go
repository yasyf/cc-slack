package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/slack-go/slack"
)

const (
	slackMaxCharts      = 2
	slackMaxChartTitle  = 50
	slackMaxChartLabel  = 20
	slackMaxChartSeries = 12
	slackMaxChartPoints = 20
)

type chartSpec struct {
	Title json.RawMessage `json:"title"`
	Mark  json.RawMessage `json:"mark"`
	Data  struct {
		Values []map[string]any `json:"values"`
	} `json:"data"`
	Encoding map[string]chartChannel `json:"encoding"`
}

type chartChannel struct {
	Field string          `json:"field"`
	Type  string          `json:"type"`
	Title json.RawMessage `json:"title"`
}

func chartBlock(source string) (*slack.DataVisualizationBlock, error) {
	var spec chartSpec
	if err := json.Unmarshal([]byte(source), &spec); err != nil {
		return nil, fmt.Errorf("its spec is no JSON object: %w", err)
	}
	if len(spec.Data.Values) == 0 {
		return nil, errors.New("its spec holds no data.values to draw")
	}
	switch mark := jsonText(spec.Mark, "type"); mark {
	case "arc":
		return spec.pie()
	case "bar", "line", "area":
		return spec.series(slack.DataVisualizationChartType(mark))
	default:
		return nil, fmt.Errorf("its mark %q is none of bar, line, area or arc", mark)
	}
}

func (s chartSpec) pie() (*slack.DataVisualizationBlock, error) {
	value, slice := s.Encoding["theta"], s.Encoding["color"]
	if value.Field == "" || slice.Field == "" {
		return nil, errors.New("its arc needs a field on theta and on color")
	}
	segments := make([]slack.DataVisualizationSegment, 0, len(s.Data.Values))
	for _, row := range s.Data.Values {
		label, err := chartLabel(row, slice.Field)
		if err != nil {
			return nil, err
		}
		number, err := chartValue(row, value.Field)
		if err != nil {
			return nil, err
		}
		segments = append(segments, slack.NewDataVisualizationSegment(label, number))
	}
	return slack.NewDataVisualizationBlock(s.title(value, slice), slack.NewDataVisualizationPieChart(segments...)), nil
}

func (s chartSpec) series(kind slack.DataVisualizationChartType) (*slack.DataVisualizationBlock, error) {
	category, value, err := s.axes()
	if err != nil {
		return nil, err
	}
	var categories []string
	var series []slack.DataVisualizationDataSeries
	for _, row := range s.Data.Values {
		label, err := chartLabel(row, category.Field)
		if err != nil {
			return nil, err
		}
		number, err := chartValue(row, value.Field)
		if err != nil {
			return nil, err
		}
		name := cmp.Or(value.label(), value.Field)
		if color := s.Encoding["color"]; color.Field != "" {
			if name, err = chartLabel(row, color.Field); err != nil {
				return nil, err
			}
		}
		if !slices.Contains(categories, label) {
			categories = append(categories, label)
		}
		at := slices.IndexFunc(series, func(drawn slack.DataVisualizationDataSeries) bool { return drawn.Name == name })
		if at < 0 {
			at = len(series)
			series = append(series, slack.NewDataVisualizationDataSeries(name))
		}
		series[at].Data = append(series[at].Data, slack.NewDataVisualizationDataPoint(label, number))
	}
	axis := slack.NewDataVisualizationAxisConfig(categories...).WithXLabel(category.label()).WithYLabel(value.label())
	var chart slack.DataVisualizationChart
	switch kind {
	case slack.DataVisualizationChartBar:
		chart = slack.NewDataVisualizationBarChart(axis, series...)
	case slack.DataVisualizationChartLine:
		chart = slack.NewDataVisualizationLineChart(axis, series...)
	case slack.DataVisualizationChartArea:
		chart = slack.NewDataVisualizationAreaChart(axis, series...)
	}
	return slack.NewDataVisualizationBlock(s.title(value, category), chart), nil
}

func (s chartSpec) axes() (chartChannel, chartChannel, error) {
	x, y := s.Encoding["x"], s.Encoding["y"]
	switch {
	case x.Field == "" || y.Field == "":
		return chartChannel{}, chartChannel{}, errors.New("its encoding needs a field on both x and y")
	case y.quantitative() && !x.quantitative():
		return x, y, nil
	case x.quantitative() && !y.quantitative():
		return y, x, nil
	}
	return chartChannel{}, chartChannel{}, errors.New("its encoding needs one quantitative axis and one axis of categories")
}

func (s chartSpec) title(value, category chartChannel) string {
	return cmp.Or(jsonText(s.Title, "text"), value.Field+" by "+category.Field)
}

func (c chartChannel) quantitative() bool {
	return c.Type == "quantitative"
}

func (c chartChannel) label() string {
	if len(c.Title) == 0 {
		return c.Field
	}
	return jsonText(c.Title, "text")
}

func jsonText(raw json.RawMessage, key string) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return ""
	}
	return jsonText(object[key], key)
}

func chartLabel(row map[string]any, field string) (string, error) {
	switch v := row[field].(type) {
	case string:
		return v, nil
	case float64, bool:
		return fmt.Sprint(v), nil
	}
	return "", fmt.Errorf("a data row's %q is %v, not a label", field, row[field])
}

func chartValue(row map[string]any, field string) (float64, error) {
	number, ok := row[field].(float64)
	if !ok {
		return 0, fmt.Errorf("a data row's %q is %v, not a number", field, row[field])
	}
	return number, nil
}

func checkChart(chart *slack.DataVisualizationBlock, refuse func(string, ...any) error) error {
	if n := runes(chart.Title); n == 0 || n > slackMaxChartTitle {
		return refuse("its title %q is %d characters, not 1 to %d", chart.Title, n, slackMaxChartTitle)
	}
	switch c := chart.Chart.(type) {
	case *slack.DataVisualizationPieChart:
		if n := len(c.Segments); n == 0 || n > slackMaxChartSeries {
			return refuse("its pie has %d slices, not 1 to %d", n, slackMaxChartSeries)
		}
		for _, segment := range c.Segments {
			if err := checkChartLabel(segment.Label, refuse); err != nil {
				return err
			}
			if segment.Value <= 0 {
				return refuse("slice %q is %v, and a slice is above 0", segment.Label, segment.Value)
			}
		}
	case *slack.DataVisualizationBarChart:
		return checkChartSeries(c.Series, c.AxisConfig, refuse)
	case *slack.DataVisualizationLineChart:
		return checkChartSeries(c.Series, c.AxisConfig, refuse)
	case *slack.DataVisualizationAreaChart:
		return checkChartSeries(c.Series, c.AxisConfig, refuse)
	}
	return nil
}

func checkChartSeries(series []slack.DataVisualizationDataSeries, axis slack.DataVisualizationAxisConfig, refuse func(string, ...any) error) error {
	switch {
	case len(series) == 0 || len(series) > slackMaxChartSeries:
		return refuse("it draws %d series, not 1 to %d", len(series), slackMaxChartSeries)
	case len(axis.Categories) == 0 || len(axis.Categories) > slackMaxChartPoints:
		return refuse("it has %d categories, not 1 to %d", len(axis.Categories), slackMaxChartPoints)
	case runes(axis.XLabel) > slackMaxChartTitle || runes(axis.YLabel) > slackMaxChartTitle:
		return refuse("an axis title runs past %d characters", slackMaxChartTitle)
	}
	for at, category := range axis.Categories {
		if err := checkChartLabel(category, refuse); err != nil {
			return err
		}
		if slices.Contains(axis.Categories[:at], category) {
			return refuse("category %q comes twice", category)
		}
	}
	categories := slices.Sorted(slices.Values(axis.Categories))
	for at, s := range series {
		if err := checkChartLabel(s.Name, refuse); err != nil {
			return err
		}
		if slices.ContainsFunc(series[:at], func(o slack.DataVisualizationDataSeries) bool { return o.Name == s.Name }) {
			return refuse("series %q comes twice", s.Name)
		}
		labels := make([]string, 0, len(s.Data))
		for _, point := range s.Data {
			labels = append(labels, point.Label)
		}
		if !slices.Equal(slices.Sorted(slices.Values(labels)), categories) {
			return refuse("series %q needs exactly one value for each category", s.Name)
		}
	}
	return nil
}

func checkChartLabel(label string, refuse func(string, ...any) error) error {
	if n := runes(label); n == 0 || n > slackMaxChartLabel {
		return refuse("label %q is %d characters, not 1 to %d", label, n, slackMaxChartLabel)
	}
	return nil
}
