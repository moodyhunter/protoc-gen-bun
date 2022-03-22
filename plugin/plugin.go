package plugin

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	bun "github.com/moodyhunter/protoc-gen-bun/options"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

var (
	ErrNotOrmable = errors.New("type is not ormable")
)

var (
	uuidImport      = "github.com/satori/go.uuid"
	pqImport        = "github.com/lib/pq"
	timestampImport = "google.golang.org/protobuf/types/known/timestamppb"
	wktImport       = "google.golang.org/protobuf/types/known/wrapperspb"
	bunImport       = "github.com/uptrace/bun"
	stdTimeImport   = "time"
	bigintImport    = "math/big"
)

var builtinTypes = map[string]struct{}{
	"bool":    {},
	"int":     {},
	"int8":    {},
	"int16":   {},
	"int32":   {},
	"int64":   {},
	"uint":    {},
	"uint8":   {},
	"uint16":  {},
	"uint32":  {},
	"uint64":  {},
	"uintptr": {},
	"float32": {},
	"float64": {},
	"string":  {},
	"[]byte":  {},
}

var wellKnownTypes = map[string]string{
	"StringValue": "*string",
	"DoubleValue": "*float64",
	"FloatValue":  "*float32",
	"Int32Value":  "*int32",
	"Int64Value":  "*int64",
	"UInt32Value": "*uint32",
	"UInt64Value": "*uint64",
	"BoolValue":   "*bool",
	//  "BytesValue" : "*[]byte",
}

const (
	protoTypeTimestamp = "Timestamp" // last segment, first will be *google_protobufX
)

// DB Engine Enum
const (
	ENGINE_UNSET = iota
	ENGINE_POSTGRES
)

type ORMBuilder struct {
	plugin         *protogen.Plugin
	ormableTypes   map[string]*OrmableType
	messages       map[string]struct{}
	currentFile    string
	currentPackage string
	dbEngine       int
	stringEnums    bool
	suppressWarn   bool
}

func New(opts protogen.Options, request *pluginpb.CodeGeneratorRequest) (*ORMBuilder, error) {
	plugin, err := opts.New(request)
	if err != nil {
		return nil, err
	}

	builder := &ORMBuilder{
		plugin:       plugin,
		ormableTypes: make(map[string]*OrmableType),
		messages:     make(map[string]struct{}),
	}

	params := parseParameter(request.GetParameter())

	if strings.EqualFold(params["engine"], "postgres") {
		builder.dbEngine = ENGINE_POSTGRES
	} else {
		builder.dbEngine = ENGINE_UNSET
	}

	if strings.EqualFold(params["enums"], "string") {
		builder.stringEnums = true
	}

	if _, ok := params["quiet"]; ok {
		builder.suppressWarn = true
	}

	return builder, nil
}

func parseParameter(param string) map[string]string {
	paramMap := make(map[string]string)

	params := strings.Split(param, ",")
	for _, param := range params {
		if strings.Contains(param, "=") {
			kv := strings.Split(param, "=")
			paramMap[kv[0]] = kv[1]
			continue
		}
		paramMap[param] = ""
	}

	return paramMap
}

type OrmableType struct {
	File       *protogen.File
	Fields     map[string]*Field
	Methods    map[string]*autogenMethod
	Name       string
	OriginName string
	Package    string
}

func NewOrmableType(originalName string, pkg string, file *protogen.File) *OrmableType {
	return &OrmableType{
		OriginName: originalName,
		Package:    pkg,
		File:       file,
		Fields:     make(map[string]*Field),
		Methods:    make(map[string]*autogenMethod),
	}
}

type Field struct {
	*bun.BUNFieldOptions
	ParentGoType   string
	Type           string
	Package        string
	ParentOrigName string
}

type autogenMethod struct {
	*protogen.Method
	outType           *protogen.Message
	inType            *protogen.Message
	verb              string
	baseType          string
	fieldMaskName     string
	ccName            string
	followsConvention bool
}

type fileImports struct {
	wktPkgName      string
	packages        map[string]*pkgImport
	typesToRegister []string
	stdImports      []string
}

type pkgImport struct {
	packagePath string
	alias       string
}

func (b *ORMBuilder) Generate() (*pluginpb.CodeGeneratorResponse, error) {
	genFileMap := make(map[string]*protogen.GeneratedFile)

	for _, protoFile := range b.plugin.Files {
		fileName := protoFile.GeneratedFilenamePrefix + ".pb.bun.go"
		g := b.plugin.NewGeneratedFile(fileName, ".")
		genFileMap[fileName] = g

		b.currentPackage = protoFile.GoImportPath.String()

		// first traverse: preload the messages
		for _, message := range protoFile.Messages {
			if message.Desc.IsMapEntry() {
				continue
			}

			typeName := string(message.Desc.Name())
			b.messages[typeName] = struct{}{}

			if isOrmable(message) {
				ormable := NewOrmableType(typeName, string(protoFile.GoPackageName), protoFile)
				b.ormableTypes[typeName] = ormable
			}
		}

		// second traverse: parse basic fields
		for _, message := range protoFile.Messages {
			if isOrmable(message) {
				b.parseBasicFields(message, g)
			}
		}

		// third traverse: build associations
		for _, message := range protoFile.Messages {
			typeName := string(message.Desc.Name())
			if isOrmable(message) {
				b.parseAssociations(message, g)
				o := b.getOrmable(typeName)
				if b.hasPrimaryKey(o) {
					_, fd := b.findPrimaryKey(o)
					fd.ParentOrigName = o.OriginName
				}
			}
		}

	}

	for _, protoFile := range b.plugin.Files {
		// generate actual code
		fileName := protoFile.GeneratedFilenamePrefix + ".pb.bun.go"
		g, ok := genFileMap[fileName]
		if !ok {
			panic("generated file should be present")
		}

		if !protoFile.Generate {
			g.Skip()
			continue
		}

		skip := true

		for _, message := range protoFile.Messages {
			if isOrmable(message) {
				skip = false
				break
			}
		}

		if skip {
			g.Skip()
			continue
		}

		g.P("package ", protoFile.GoPackageName)

		for _, message := range protoFile.Messages {
			if isOrmable(message) {
				b.generateOrmable(g, message)
				b.generateConvertFunctions(g, message)
				b.generateHookInterfaces(g, message)
			}
		}
	}
	resp := b.plugin.Response()

	var SupportedFeatures = uint64(pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL)
	resp.SupportedFeatures = proto.Uint64(SupportedFeatures)

	return resp, nil
}

func (b *ORMBuilder) generateConvertFunctions(g *protogen.GeneratedFile, message *protogen.Message) {
	typeName := string(message.Desc.Name())
	ormable := b.getOrmable(camelCase(typeName))

	///// To Orm
	g.P(`// ToORM runs the BeforeToORM hook if present, converts the fields of this`)
	g.P(`// object to ORM format, runs the AfterToORM hook, then returns the ORM object`)
	g.P(`func (m *`, typeName, `) ToORM (ctx `, generateImport("Context", "context", g), `) (`, typeName, `ORM, error) {`)
	g.P(`to := `, typeName, `ORM{}`)
	g.P(`var err error`)
	g.P(`if prehook, ok := interface{}(m).(`, typeName, `WithBeforeToORM); ok {`)
	g.P(`if err = prehook.BeforeToORM(ctx, &to); err != nil {`)
	g.P(`return to, err`)
	g.P(`}`)
	g.P(`}`)
	for _, field := range message.Fields {
		ofield := ormable.Fields[camelCase(field.GoName)]
		b.generateFieldConversion(message, field, true, ofield, g)
	}

	g.P(`if posthook, ok := interface{}(m).(`, typeName, `WithAfterToORM); ok {`)
	g.P(`err = posthook.AfterToORM(ctx, &to)`)
	g.P(`}`)
	g.P(`return to, err`)
	g.P(`}`)

	g.P()
	///// To Pb
	g.P(`// ToPB runs the BeforeToPB hook if present, converts the fields of this`)
	g.P(`// object to PB format, runs the AfterToPB hook, then returns the PB object`)
	g.P(`func (m *`, typeName, `ORM) ToPB (ctx context.Context) (*`, typeName, `, error) {`)
	g.P(`to := &`, typeName, `{}`)
	g.P(`var err error`)
	g.P(`if prehook, ok := interface{}(m).(`, typeName, `WithBeforeToPB); ok {`)
	g.P(`if err = prehook.BeforeToPB(ctx, to); err != nil {`)
	g.P(`return to, err`)
	g.P(`}`)
	g.P(`}`)
	for _, field := range message.Fields {
		ofield := ormable.Fields[camelCase(field.GoName)]
		b.generateFieldConversion(message, field, false, ofield, g)
	}
	g.P(`if posthook, ok := interface{}(m).(`, typeName, `WithAfterToPB); ok {`)
	g.P(`err = posthook.AfterToPB(ctx, to)`)
	g.P(`}`)
	g.P(`return to, err`)
	g.P(`}`)
}

func (b *ORMBuilder) generateOrmable(g *protogen.GeneratedFile, message *protogen.Message) {
	msgOptions := getMessageOptions(message)
	ormable := b.getOrmable(message.GoIdent.GoName)
	generateImport("BaseModel", bunImport, g)
	g.P(`type `, ormable.Name, ` struct {`)

	g.P("bun.BaseModel ", "`", `bun:"table:`, msgOptions.Table, `"`, "`")

	var names []string
	for name := range ormable.Fields {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		field := ormable.Fields[name]
		sp := strings.Split(field.Type, ".")

		if len(sp) == 2 && sp[1] == "UUID" {
			s := generateImport("UUID", uuidImport, g)
			if field.Type[0] == '*' {
				field.Type = "*" + s
			} else {
				field.Type = s
			}
		}

		if len(sp) == 2 && sp[1] == "BigInt" {
			s := generateImport("BigInt", bigintImport, g)
			if field.Type[0] == '*' {
				field.Type = "*" + s
			} else {
				field.Type = s
			}
		}

		g.P(name, ` `, field.Type, b.renderBUNTag(field))
	}

	g.P(`}`)
	g.P()
}

func (b *ORMBuilder) parseAssociations(msg *protogen.Message, g *protogen.GeneratedFile) {
	typeName := camelCase(string(msg.Desc.Name())) // TODO: camelSnakeCase
	ormable := b.getOrmable(typeName)

	for _, field := range msg.Fields {
		options := field.Desc.Options().(*descriptorpb.FieldOptions)
		fieldOpts := getFieldOptions(options)

		fieldName := camelCase(string(field.Desc.Name()))
		var fieldType string

		if field.Desc.Message() == nil {
			fieldType = field.Desc.Kind().String() // was GoType
		} else {
			fieldType = string(field.Desc.Message().Name())
		}

		fieldType = strings.Trim(fieldType, "[]*")

		if b.isOrmable(fieldType) {
			if fieldOpts == nil {
				fieldOpts = &bun.BUNFieldOptions{}
			}

			if field.Message != nil {
				fieldType = b.typeName(field.Message.GoIdent, g)
			}

			// Register type used, in case it's an imported type from another package
			// b.GetFileImports().typesToRegister = append(b.GetFileImports().typesToRegister, fieldType) // maybe we need other fields type
			ormable.Fields[fieldName] = &Field{Type: fieldType, BUNFieldOptions: fieldOpts}
		}
	}
}

func (b *ORMBuilder) hasPrimaryKey(ormable *OrmableType) bool {
	for _, field := range ormable.Fields {
		if field.GetPrimaryKey() {
			return true
		}
	}
	for fieldName := range ormable.Fields {
		if strings.ToLower(fieldName) == "id" {
			return true
		}
	}
	return false
}

func (b *ORMBuilder) isOrmable(typeName string) bool {
	_, ok := b.ormableTypes[typeName]
	return ok
}

func (b *ORMBuilder) findPrimaryKey(ormable *OrmableType) (string, *Field) {
	for fieldName, field := range ormable.Fields {
		if field.GetPrimaryKey() {
			return fieldName, field
		}
	}
	for fieldName, field := range ormable.Fields {
		if strings.ToLower(fieldName) == "id" {
			return fieldName, field
		}
	}

	panic("no primary_key")
}

func (b *ORMBuilder) getOrmable(typeName string) *OrmableType {
	r, err := GetOrmable(b.ormableTypes, typeName)
	if err != nil {
		panic(err)
	}

	return r
}

func (b *ORMBuilder) parseBasicFields(msg *protogen.Message, g *protogen.GeneratedFile) {
	typeName := string(msg.Desc.Name())
	ormable, ok := b.ormableTypes[typeName]
	if !ok {
		panic("typeName should be found")
	}
	ormable.Name = fmt.Sprintf("%sORM", typeName) // TODO: there are no reason to do it here

	for _, field := range msg.Fields {
		fd := field.Desc
		options := fd.Options().(*descriptorpb.FieldOptions)
		bunOptions := getFieldOptions(options)
		if bunOptions == nil {
			bunOptions = &bun.BUNFieldOptions{}
		}

		tag := bunOptions
		fieldName := camelCase(string(fd.Name()))
		fieldType := fd.Kind().String()

		var typePackage string

		if b.dbEngine == ENGINE_POSTGRES && b.IsAbleToMakePQArray(fieldType) && field.Desc.IsList() {
			switch fieldType {
			case "bool":
				fieldType = generateImport("BoolArray", pqImport, g)
				bunOptions = tagWithType(tag, "bool[]")
			case "double":
				fieldType = generateImport("Float64Array", pqImport, g)
				bunOptions = tagWithType(tag, "float[]")
			case "int64":
				fieldType = generateImport("Int64Array", pqImport, g)
				bunOptions = tagWithType(tag, "integer[]")
			case "string":
				fieldType = generateImport("StringArray", pqImport, g)
				bunOptions = tagWithType(tag, "text[]")
			default:
				continue
			}
		} else if (field.Message == nil || !b.isOrmable(fieldType)) && field.Desc.IsList() {
			// not implemented
			continue
		} else if field.Enum != nil {
			fieldType = "int32"
			if b.stringEnums {
				fieldType = "string"
			}
		} else if field.Message != nil {
			xs := strings.Split(string(field.Message.Desc.FullName()), ".")
			rawType := xs[len(xs)-1]

			if v, ok := wellKnownTypes[rawType]; ok {
				fieldType = v
			} else if rawType == protoTypeTimestamp {
				typePackage = stdTimeImport
				fieldType = "*" + generateImport("Time", stdTimeImport, g)
			}
		}

		switch fieldType {
		case "float":
			fieldType = "float32"
		case "double":
			fieldType = "float64"
		}

		f := &Field{
			BUNFieldOptions: bunOptions,
			ParentGoType:    "",
			Type:            fieldType,
			Package:         typePackage,
		}

		ormable.Fields[fieldName] = f
	}

}

func getFieldOptions(options *descriptorpb.FieldOptions) *bun.BUNFieldOptions {
	if options == nil {
		return nil
	}

	v := proto.GetExtension(options, bun.E_Field)
	if v == nil {
		return nil
	}

	opts, ok := v.(*bun.BUNFieldOptions)
	if !ok {
		return nil
	}

	return opts
}

func getMessageOptions(message *protogen.Message) *bun.BUNMessageOptions {
	options := message.Desc.Options()
	if options == nil {
		return nil
	}
	v := proto.GetExtension(options, bun.E_Opts)
	if v == nil {
		return nil
	}

	opts, ok := v.(*bun.BUNMessageOptions)
	if !ok {
		return nil
	}

	return opts
}

func isOrmable(message *protogen.Message) bool {
	desc := message.Desc
	options := desc.Options()

	m, ok := proto.GetExtension(options, bun.E_Opts).(*bun.BUNMessageOptions)
	if !ok || m == nil {
		return false
	}

	return m.Ormable
}

func (b *ORMBuilder) IsAbleToMakePQArray(fieldType string) bool {
	switch fieldType {
	case "bool", "double", "int64", "string":
		return true
	default:
		return false
	}
}

func tagWithType(tag *bun.BUNFieldOptions, typename string) *bun.BUNFieldOptions {
	if tag == nil {
		tag = &bun.BUNFieldOptions{}
	}

	tag.Dbtype = typename
	return tag
}

func camelCase(s string) string {
	if s == "" {
		return ""
	}
	t := make([]byte, 0, 32)
	i := 0
	if s[0] == '_' {
		// Need a capital letter; drop the '_'.
		t = append(t, 'X')
		i++
	}
	// Invariant: if the next letter is lower case, it must be converted
	// to upper case.
	// That is, we process a word at a time, where words are marked by _ or
	// upper case letter. Digits are treated as words.
	for ; i < len(s); i++ {
		c := s[i]
		if c == '_' && i+1 < len(s) && isASCIILower(s[i+1]) {
			continue // Skip the underscore in s.
		}
		if isASCIIDigit(c) {
			t = append(t, c)
			continue
		}
		// Assume we have a letter now - if not, it's a bogus identifier.
		// The next word is a sequence of characters that must start upper case.
		if isASCIILower(c) {
			c ^= ' ' // Make it a capital letter.
		}
		t = append(t, c) // Guaranteed not lower case.
		// Accept lower case sequence that follows.
		for i+1 < len(s) && isASCIILower(s[i+1]) {
			i++
			t = append(t, s[i])
		}
	}
	return string(t)
}

func isASCIILower(c byte) bool {
	return 'a' <= c && c <= 'z'
}

func isASCIIDigit(c byte) bool {
	return '0' <= c && c <= '9'
}

func (b *ORMBuilder) renderBUNTag(field *Field) string {
	var bunRes string

	if len(field.Column) > 0 {
		bunRes += fmt.Sprintf("%s,", field.GetColumn())
	}
	if len(field.Dbtype) > 0 {
		bunRes += fmt.Sprintf("type:%s,", field.GetDbtype())
	}
	if field.GetPrimaryKey() {
		bunRes += "pk,"
	}
	if field.GetUnique() {
		bunRes += "unique,"
	}
	if len(field.Default) > 0 {
		bunRes += fmt.Sprintf("default:%s,", field.GetDefault())
	}
	if field.GetNotNull() {
		bunRes += "notnull,"
	}
	if field.GetAutoIncrement() {
		bunRes += "autoincrement,"
	}
	if len(field.EmbedPrefix) > 0 {
		bunRes += fmt.Sprintf("embed:%s,", field.GetEmbedPrefix())
	}

	var bunTag, atlasTag string
	if bunRes != "" {
		bunTag = fmt.Sprintf("bun:\"%s\"", strings.TrimRight(bunRes, ";"))
	}
	finalTag := strings.TrimSpace(strings.Join([]string{bunTag, atlasTag}, " "))
	if finalTag == "" {
		return ""
	} else {
		return fmt.Sprintf("`%s`", finalTag)
	}
}

// Output code that will convert a field to/from orm.
func (b *ORMBuilder) generateFieldConversion(message *protogen.Message, field *protogen.Field,
	toORM bool, ofield *Field, g *protogen.GeneratedFile) error {

	fieldName := camelCase(string(field.Desc.Name()))
	fieldType := field.Desc.Kind().String() // was GoType
	if field.Desc.Message() != nil {
		parts := strings.Split(string(field.Desc.Message().FullName()), ".")
		fieldType = parts[len(parts)-1]
	}
	if field.Desc.Cardinality() == protoreflect.Repeated {
		// Some repeated fields can be handled by github.com/lib/pq
		if b.dbEngine == ENGINE_POSTGRES && b.IsAbleToMakePQArray(fieldType) && field.Desc.IsList() {
			g.P(`if m.`, fieldName, ` != nil {`)
			switch fieldType {
			case "bool":
				g.P(`to.`, fieldName, ` = make(`, generateImport("BoolArray", pqImport, g), `, len(m.`, fieldName, `))`)
			case "double":
				g.P(`to.`, fieldName, ` = make(`, generateImport("Float64Array", pqImport, g), `, len(m.`, fieldName, `))`)
			case "int64":
				g.P(`to.`, fieldName, ` = make(`, generateImport("Int64Array", pqImport, g), `, len(m.`, fieldName, `))`)
			case "string":
				g.P(`to.`, fieldName, ` = make(`, generateImport("StringArray", pqImport, g), `, len(m.`, fieldName, `))`)
			}
			g.P(`copy(to.`, fieldName, `, m.`, fieldName, `)`)
			g.P(`}`)
		} else if b.isOrmable(fieldType) { // Repeated ORMable type
			//fieldType = strings.Trim(fieldType, "[]*")

			g.P(`for _, v := range m.`, fieldName, ` {`)
			g.P(`if v != nil {`)
			if toORM {
				g.P(`if temp`, fieldName, `, cErr := v.ToORM(ctx); cErr == nil {`)
			} else {
				g.P(`if temp`, fieldName, `, cErr := v.ToPB(ctx); cErr == nil {`)
			}
			g.P(`to.`, fieldName, ` = append(to.`, fieldName, `, &temp`, fieldName, `)`)
			g.P(`} else {`)
			g.P(`return to, cErr`)
			g.P(`}`)
			g.P(`} else {`)
			g.P(`to.`, fieldName, ` = append(to.`, fieldName, `, nil)`)
			g.P(`}`)
			g.P(`}`) // end repeated for
		} else {
			g.P(`// Repeated type `, fieldType, ` is not an ORMable message type`)
		}
	} else if field.Enum != nil { // Singular Enum, which is an int32 ---
		fieldType = b.typeName(field.Enum.GoIdent, g)
		if toORM {
			if b.stringEnums {
				g.P(`to.`, fieldName, ` = `, fieldType, `_name[int32(m.`, fieldName, `)]`)
			} else {
				g.P(`to.`, fieldName, ` = int32(m.`, fieldName, `)`)
			}
		} else {
			if b.stringEnums {
				g.P(`to.`, fieldName, ` = `, fieldType, `(`, fieldType, `_value[m.`, fieldName, `])`)
			} else {
				g.P(`to.`, fieldName, ` = `, fieldType, `(m.`, fieldName, `)`)
			}
		}
	} else if field.Message != nil { // Singular Object -------------
		//Check for WKTs
		// Type is a WKT, convert to/from as ptr to base type
		if _, exists := wellKnownTypes[fieldType]; exists { // Singular WKT -----
			if toORM {
				g.P(`if m.`, fieldName, ` != nil {`)
				g.P(`v := m.`, fieldName, `.Value`)
				g.P(`to.`, fieldName, ` = &v`)
				g.P(`}`)
			} else {
				g.P(`if m.`, fieldName, ` != nil {`)
				// g.P(`to.`, fieldName, ` = &`, b.GetFileImports().wktPkgName, ".", fieldType,
				// 	`{Value: *m.`, fieldName, `}`)
				g.P(`to.`, fieldName, ` = &`, generateImport(fieldType, wktImport, g),
					`{Value: *m.`, fieldName, `}`)
				g.P(`}`)
			}
		} else if fieldType == protoTypeTimestamp { // Singular WKT Timestamp ---
			if toORM {
				g.P(`if m.`, fieldName, ` != nil {`)
				g.P(`t := m.`, fieldName, `.AsTime()`)
				g.P(`to.`, fieldName, ` = &t`)
				g.P(`}`)
			} else {
				g.P(`if m.`, fieldName, ` != nil {`)
				g.P(`to.`, fieldName, ` = `, generateImport("New", timestampImport, g), `(*m.`, fieldName, `)`)
				g.P(`}`)
			}
		} else if b.isOrmable(fieldType) {
			// Not a WKT, but a type we're building converters for
			g.P(`if m.`, fieldName, ` != nil {`)
			if toORM {
				g.P(`temp`, fieldName, `, err := m.`, fieldName, `.ToORM (ctx)`)
			} else {
				g.P(`temp`, fieldName, `, err := m.`, fieldName, `.ToPB (ctx)`)
			}
			g.P(`if err != nil {`)
			g.P(`return to, err`)
			g.P(`}`)
			g.P(`to.`, fieldName, ` = &temp`, fieldName)
			g.P(`}`)
		}
	} else { // Singular raw ----------------------------------------------------
		g.P(`to.`, fieldName, ` = m.`, fieldName)
	}
	return nil
}

func (b *ORMBuilder) generateBeforeHookCall(orm *OrmableType, method string, g *protogen.GeneratedFile) {
	g.P(`if hook, ok := interface{}(&ormObj).(`, orm.Name, `WithBefore`, method, `); ok {`)
	g.P(`if db, err = hook.Before`, method, `(ctx, db); err != nil {`)
	g.P(`return nil, err`)
	g.P(`}`)
	g.P(`}`)
}

func (b *ORMBuilder) generateAfterHookCall(orm *OrmableType, method string, g *protogen.GeneratedFile) {
	g.P(`if hook, ok := interface{}(&ormObj).(`, orm.Name, `WithAfter`, method, `); ok {`)
	g.P(`if err = hook.After`, method, `(ctx, db); err != nil {`)
	g.P(`return nil, err`)
	g.P(`}`)
	g.P(`}`)
}

func (b *ORMBuilder) generateHookInterfaces(g *protogen.GeneratedFile, message *protogen.Message) {
	typeName := string(message.Desc.Name())
	g.P(`// The following are interfaces you can implement for special behavior during ORM/PB conversions`)
	g.P(`// of type `, typeName, ` the arg will be the target, the caller the one being converted from`)
	g.P()
	for _, desc := range [][]string{
		{"BeforeToORM", typeName + "ORM", " called before default ToORM code"},
		{"AfterToORM", typeName + "ORM", " called after default ToORM code"},
		{"BeforeToPB", typeName, " called before default ToPB code"},
		{"AfterToPB", typeName, " called after default ToPB code"},
	} {
		g.P(`// `, typeName, desc[0], desc[2])
		g.P(`type `, typeName, `With`, desc[0], ` interface {`)
		g.P(desc[0], `(context.Context, *`, desc[1], `) error`)
		g.P(`}`)
		g.P()
	}
}

func generateImport(name string, importPath string, g *protogen.GeneratedFile) string {
	return g.QualifiedGoIdent(protogen.GoIdent{
		GoName:       name,
		GoImportPath: protogen.GoImportPath(importPath),
	})
}

func (b *ORMBuilder) typeName(ident protogen.GoIdent, g *protogen.GeneratedFile) string {
	// drop package prefix, no need to import
	if b.currentPackage == ident.GoImportPath.String() {
		return ident.GoName
	}

	return generateImport(ident.GoName, string(ident.GoImportPath), g)
}

func GetOrmable(ormableTypes map[string]*OrmableType, typeName string) (*OrmableType, error) {
	parts := strings.Split(typeName, ".")
	ormable, ok := ormableTypes[strings.TrimSuffix(strings.Trim(parts[len(parts)-1], "[]*"), "ORM")]
	var err error
	if !ok {
		err = ErrNotOrmable
	}
	return ormable, err
}

func (b *ORMBuilder) sameType(field1 *Field, field2 *Field) bool {
	isPointer1 := strings.HasPrefix(field1.Type, "*")
	typeParts1 := strings.Split(field1.Type, ".")

	if len(typeParts1) == 2 {
		isPointer2 := strings.HasPrefix(field2.Type, "*")
		typeParts2 := strings.Split(field2.Type, ".")

		if len(typeParts2) == 2 && isPointer1 == isPointer2 && typeParts1[1] == typeParts2[1] && field1.Package == field2.Package {
			return true
		}

		return false
	}

	return field1.Type == field2.Type
}

func getFieldType(field *protogen.Field) string {
	if field.Desc.Message() == nil {
		return field.Desc.Kind().String()
	}

	return string(field.Desc.Message().Name())
}

func getFieldIdent(field *protogen.Field) protogen.GoIdent {
	if field.Desc.Message() != nil {
		return field.Message.GoIdent
	}

	return field.GoIdent
}
